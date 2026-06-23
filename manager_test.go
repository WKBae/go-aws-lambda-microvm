package microvm

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestBuildImagePackagesFSUploadsAndCreatesImage(t *testing.T) {
	s3Client := &fakeS3{}
	mvmClient := &fakeMicroVMs{
		createImageOutput: &lambdamicrovms.CreateMicrovmImageOutput{
			ImageArn:     ptr("arn:image"),
			Name:         ptr("test-image"),
			ImageVersion: ptr("1.0"),
			State:        types.MicrovmImageStateCreating,
		},
		getImageOutputs: []*lambdamicrovms.GetMicrovmImageOutput{{
			ImageArn:                 ptr("arn:image"),
			Name:                     ptr("test-image"),
			LatestActiveImageVersion: ptr("1.0"),
			State:                    types.MicrovmImageStateCreated,
		}},
	}
	manager := testManager(s3Client, mvmClient)

	image, err := manager.BuildImage(context.Background(), BuildImageInput{
		Name: "test-image",
		FS: fstest.MapFS{
			"app/Dockerfile":     &fstest.MapFile{Data: []byte("FROM scratch\n")},
			"app/main.go":        &fstest.MapFile{Data: []byte("package main\n")},
			"app/node_modules/x": &fstest.MapFile{Data: []byte("ignored")},
		},
		Root:             "app",
		ArtifactKey:      "artifacts/test.zip",
		ResourcesMiB:     1024,
		Wait:             true,
		WaitPollInterval: time.Nanosecond,
		Exclude: func(name string, entry fs.DirEntry) bool {
			return name == "app/node_modules"
		},
	})
	if err != nil {
		t.Fatalf("BuildImage returned error: %v", err)
	}
	if image.ARN != "arn:image" || image.Version != "1.0" || image.ArtifactURI != "s3://bucket/artifacts/test.zip" {
		t.Fatalf("unexpected image: %+v", image)
	}

	if got := deref(s3Client.putInput.Bucket); got != "bucket" {
		t.Fatalf("bucket = %q", got)
	}
	if got := deref(s3Client.putInput.Key); got != "artifacts/test.zip" {
		t.Fatalf("key = %q", got)
	}
	zipBytes, err := io.ReadAll(s3Client.putInput.Body)
	if err != nil {
		t.Fatalf("read uploaded body: %v", err)
	}
	assertZipContains(t, zipBytes, map[string]string{
		"Dockerfile": "FROM scratch\n",
		"main.go":    "package main\n",
	})
	assertZipOmits(t, zipBytes, "node_modules/x")

	create := mvmClient.createImageInput
	if got := deref(create.Name); got != "test-image" {
		t.Fatalf("create name = %q", got)
	}
	if got := create.CodeArtifact.(*types.CodeArtifactMemberUri).Value; got != "s3://bucket/artifacts/test.zip" {
		t.Fatalf("artifact uri = %q", got)
	}
	if got := *create.Resources[0].MinimumMemoryInMiB; got != 1024 {
		t.Fatalf("resources memory = %d", got)
	}
}

func TestRunWaitsCreatesTokenAndSessionAddsHeaders(t *testing.T) {
	var gotAuth, gotPort string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(authHeader)
		gotPort = r.Header.Get(portHeader)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	mvmClient := &fakeMicroVMs{
		runOutput: &lambdamicrovms.RunMicrovmOutput{
			MicrovmId:    ptr("mvm-1"),
			Endpoint:     ptr(server.Listener.Addr().String()),
			ImageArn:     ptr("arn:image"),
			ImageVersion: ptr("1.0"),
			State:        types.MicrovmStatePending,
		},
		getMicroVMOutputs: []*lambdamicrovms.GetMicrovmOutput{{
			MicrovmId:    ptr("mvm-1"),
			Endpoint:     ptr(server.Listener.Addr().String()),
			ImageArn:     ptr("arn:image"),
			ImageVersion: ptr("1.0"),
			State:        types.MicrovmStateRunning,
		}},
		tokenOutput: &lambdamicrovms.CreateMicrovmAuthTokenOutput{
			AuthToken: map[string]string{authHeader: "token-value"},
		},
	}
	manager := testManager(&fakeS3{}, mvmClient)
	manager.httpClient = server.Client()

	session, err := manager.Run(context.Background(), RunInput{
		ImageIdentifier:  "arn:image",
		Wait:             true,
		WaitPollInterval: time.Nanosecond,
		AllowedPorts: []types.PortSpecification{
			&types.PortSpecificationMemberPort{Value: 9090},
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	resp, err := session.Get(context.Background(), "/health", 9090)
	if err != nil {
		t.Fatalf("session Get returned error: %v", err)
	}
	_ = resp.Body.Close()

	if gotAuth != "token-value" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotPort != "9090" {
		t.Fatalf("port header = %q", gotPort)
	}
	if got := mvmClient.tokenInput.AllowedPorts[0].(*types.PortSpecificationMemberPort).Value; got != 9090 {
		t.Fatalf("token allowed port = %d", got)
	}
}

func testManager(s3Client *fakeS3, mvmClient *fakeMicroVMs) *Manager {
	return &Manager{
		s3:             s3Client,
		mvm:            mvmClient,
		httpClient:     http.DefaultClient,
		bucket:         "bucket",
		artifactPrefix: "prefix",
		baseImageARN:   "arn:base",
		buildRoleARN:   "arn:role",
		defaultIngress: []string{"ingress"},
		defaultEgress:  []string{"egress"},
		pollInterval:   time.Nanosecond,
	}
}

type fakeS3 struct {
	putInput *s3.PutObjectInput
}

func (f *fakeS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putInput = input
	return &s3.PutObjectOutput{}, nil
}

type fakeMicroVMs struct {
	createImageInput  *lambdamicrovms.CreateMicrovmImageInput
	createImageOutput *lambdamicrovms.CreateMicrovmImageOutput
	getImageOutputs   []*lambdamicrovms.GetMicrovmImageOutput

	runInput          *lambdamicrovms.RunMicrovmInput
	runOutput         *lambdamicrovms.RunMicrovmOutput
	getMicroVMOutputs []*lambdamicrovms.GetMicrovmOutput
	tokenInput        *lambdamicrovms.CreateMicrovmAuthTokenInput
	tokenOutput       *lambdamicrovms.CreateMicrovmAuthTokenOutput
}

func (f *fakeMicroVMs) CreateMicrovmImage(_ context.Context, input *lambdamicrovms.CreateMicrovmImageInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmImageOutput, error) {
	f.createImageInput = input
	return f.createImageOutput, nil
}

func (f *fakeMicroVMs) GetMicrovmImage(_ context.Context, _ *lambdamicrovms.GetMicrovmImageInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmImageOutput, error) {
	out := f.getImageOutputs[0]
	f.getImageOutputs = f.getImageOutputs[1:]
	return out, nil
}

func (f *fakeMicroVMs) RunMicrovm(_ context.Context, input *lambdamicrovms.RunMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	f.runInput = input
	return f.runOutput, nil
}

func (f *fakeMicroVMs) GetMicrovm(_ context.Context, _ *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	out := f.getMicroVMOutputs[0]
	f.getMicroVMOutputs = f.getMicroVMOutputs[1:]
	return out, nil
}

func (f *fakeMicroVMs) CreateMicrovmAuthToken(_ context.Context, input *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	f.tokenInput = input
	return f.tokenOutput, nil
}

func (f *fakeMicroVMs) SuspendMicrovm(context.Context, *lambdamicrovms.SuspendMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.SuspendMicrovmOutput, error) {
	return &lambdamicrovms.SuspendMicrovmOutput{}, nil
}

func (f *fakeMicroVMs) ResumeMicrovm(context.Context, *lambdamicrovms.ResumeMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error) {
	return &lambdamicrovms.ResumeMicrovmOutput{}, nil
}

func (f *fakeMicroVMs) TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

func assertZipContains(t *testing.T, data []byte, want map[string]string) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	got := map[string]string{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", file.Name, err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %q: %v", file.Name, err)
		}
		got[file.Name] = string(body)
	}
	for name, body := range want {
		if got[name] != body {
			t.Fatalf("zip entry %q = %q, want %q", name, got[name], body)
		}
	}
}

func assertZipOmits(t *testing.T, data []byte, name string) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, file := range reader.File {
		if file.Name == name {
			t.Fatalf("zip unexpectedly contains %q", name)
		}
	}
}
