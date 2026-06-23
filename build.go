package microvm

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// BuildImageInput describes a MicroVM image build from an fs.FS.
//
// BuildImage packages FS into a zip artifact, uploads it to S3, and calls
// CreateMicrovmImage with the resulting s3:// URI. The selected Root must
// contain a Dockerfile at its top level because Lambda executes that Dockerfile
// while creating the MicroVM image snapshot.
type BuildImageInput struct {
	// Name is the MicroVM image name. It must be unique within the AWS account
	// and is passed to CreateMicrovmImageInput.Name.
	Name string
	// FS is the source filesystem to package into the MicroVM image artifact.
	// It may be an os.DirFS, embed.FS, fstest.MapFS, or any other fs.FS.
	FS fs.FS
	// Root is the directory inside FS to package. The default is ".". The
	// Dockerfile must be located at Root/Dockerfile and is written to the zip
	// root as Dockerfile.
	Root string

	// ArtifactBucket overrides Config.ArtifactBucket for this build. The bucket
	// should be in the same AWS Region as the MicroVM image build.
	ArtifactBucket string
	// ArtifactKey is the S3 object key for the generated zip artifact. If empty,
	// BuildImage generates one from Config.ArtifactPrefix, Name, and a timestamp.
	ArtifactKey string

	// BaseImageARN overrides Config.BaseImageARN for this build. It must be the
	// ARN of a Lambda-managed MicroVM base image.
	BaseImageARN string
	// BuildRoleARN overrides Config.BuildRoleARN for this build. Lambda assumes
	// this role to download the S3 artifact and write build logs.
	BuildRoleARN string

	// Description is an optional human-readable description for the image
	// version created by this build.
	Description string
	// EnvironmentVariables are injected into the MicroVM runtime environment at
	// image build time. Values are shared by all MicroVMs launched from the
	// resulting image version.
	EnvironmentVariables map[string]string
	// Tags are attached to the created MicroVM image resource for organization,
	// cost allocation, and IAM attribute-based access control.
	Tags map[string]string
	// Hooks configures build-time hooks such as ready and validate, and runtime
	// hooks such as run, resume, suspend, and terminate. The application must
	// listen on the configured hook port.
	Hooks *types.Hooks
	// Logging configures build-time and runtime CloudWatch Logs output, or
	// disables logging. Leave nil to use the service default.
	Logging types.Logging
	// ResourcesMiB sets the baseline MicroVM memory in MiB. Lambda scales vCPU
	// proportionally with memory and can vertically scale above this baseline.
	// If zero, the service default is used.
	ResourcesMiB int32
	// AdditionalOsCapabilities grants additional Linux capabilities within the
	// MicroVM isolation boundary. The current service-supported value is
	// types.CapabilityAll.
	AdditionalOsCapabilities []types.Capability
	// EgressNetworkConnectors lists network connector ARNs made available to
	// MicroVMs launched from the image at runtime.
	EgressNetworkConnectors []string

	// Exclude is called for each fs.WalkDir entry before packaging. Returning
	// true skips a file; returning true for a directory skips the entire subtree.
	Exclude func(name string, entry fs.DirEntry) bool

	// Wait tells BuildImage to poll GetMicrovmImage until the image reaches
	// CREATED or UPDATED, or until a failed terminal state is observed.
	Wait bool
	// WaitPollInterval overrides the Manager polling interval for this build.
	// If zero, the Manager default is used.
	WaitPollInterval time.Duration

	// MutateCreateInput can adjust the generated AWS SDK input immediately before
	// CreateMicrovmImage is called. Use it for advanced options not represented
	// directly by this convenience API.
	MutateCreateInput func(*lambdamicrovms.CreateMicrovmImageInput)
}

// BuildImageFromDirInput describes a MicroVM image build from a local
// directory.
//
// It mirrors BuildImageInput but replaces FS and Root with Dir. BuildImageFromDir
// packages os.DirFS(Dir), so Dir itself must contain the Dockerfile that should
// appear at the artifact zip root.
type BuildImageFromDirInput struct {
	// Name is the MicroVM image name. It must be unique within the AWS account.
	Name string
	// Dir is the local directory to package. It must contain a Dockerfile at its
	// top level.
	Dir string

	// ArtifactBucket overrides Config.ArtifactBucket for this build.
	ArtifactBucket string
	// ArtifactKey is the S3 object key for the generated zip artifact. If empty,
	// BuildImageFromDir generates one from Config.ArtifactPrefix, Name, and a
	// timestamp.
	ArtifactKey string

	// BaseImageARN overrides Config.BaseImageARN for this build.
	BaseImageARN string
	// BuildRoleARN overrides Config.BuildRoleARN for this build.
	BuildRoleARN string

	// Description is an optional human-readable description for the image
	// version created by this build.
	Description string
	// EnvironmentVariables are injected into the MicroVM runtime environment at
	// image build time and are shared by MicroVMs launched from this version.
	EnvironmentVariables map[string]string
	// Tags are attached to the created MicroVM image resource.
	Tags map[string]string
	// Hooks configures build-time and runtime lifecycle hooks.
	Hooks *types.Hooks
	// Logging configures CloudWatch Logs output or disables logging.
	Logging types.Logging
	// ResourcesMiB sets the baseline MicroVM memory in MiB. If zero, the service
	// default is used.
	ResourcesMiB int32
	// AdditionalOsCapabilities grants additional Linux capabilities inside the
	// MicroVM isolation boundary.
	AdditionalOsCapabilities []types.Capability
	// EgressNetworkConnectors lists runtime egress connector ARNs available to
	// MicroVMs launched from this image.
	EgressNetworkConnectors []string

	// Exclude is called for each directory entry before packaging.
	Exclude func(name string, entry fs.DirEntry) bool

	// Wait tells BuildImageFromDir to poll until the image is ready or failed.
	Wait bool
	// WaitPollInterval overrides the Manager polling interval for this build.
	WaitPollInterval time.Duration

	// MutateCreateInput can adjust the generated AWS SDK input immediately before
	// CreateMicrovmImage is called.
	MutateCreateInput func(*lambdamicrovms.CreateMicrovmImageInput)
}

// Image is the high-level result of a MicroVM image creation or lookup.
type Image struct {
	// ARN is the AWS ARN of the MicroVM image resource.
	ARN string
	// Name is the MicroVM image name.
	Name string
	// Version is the image version created by CreateMicrovmImage, or the latest
	// active version returned by GetMicrovmImage.
	Version string
	// State is the current MicroVM image lifecycle state, such as CREATING,
	// CREATED, UPDATED, or CREATE_FAILED.
	State types.MicrovmImageState
	// ArtifactURI is the s3:// URI of the zip artifact uploaded by BuildImage or
	// BuildImageFromDir. It is empty for images returned only from GetMicrovmImage.
	ArtifactURI string
}

// BuildImageFromDir packages a local directory and creates a MicroVM image.
func (m *Manager) BuildImageFromDir(ctx context.Context, in BuildImageFromDirInput) (*Image, error) {
	if in.Dir == "" {
		return nil, errors.New("microvm: BuildImageFromDirInput.Dir is required")
	}
	return m.BuildImage(ctx, BuildImageInput{
		Name:                     in.Name,
		FS:                       os.DirFS(in.Dir),
		ArtifactBucket:           in.ArtifactBucket,
		ArtifactKey:              in.ArtifactKey,
		BaseImageARN:             in.BaseImageARN,
		BuildRoleARN:             in.BuildRoleARN,
		Description:              in.Description,
		EnvironmentVariables:     in.EnvironmentVariables,
		Tags:                     in.Tags,
		Hooks:                    in.Hooks,
		Logging:                  in.Logging,
		ResourcesMiB:             in.ResourcesMiB,
		AdditionalOsCapabilities: in.AdditionalOsCapabilities,
		EgressNetworkConnectors:  in.EgressNetworkConnectors,
		Exclude:                  in.Exclude,
		Wait:                     in.Wait,
		WaitPollInterval:         in.WaitPollInterval,
		MutateCreateInput:        in.MutateCreateInput,
	})
}

// BuildImage packages an fs.FS, uploads it to S3, and creates a MicroVM image.
//
// The generated artifact zip contains files under Root at the zip root. Lambda
// downloads the artifact, executes the Dockerfile, starts the application, waits
// for configured image hooks, and snapshots the initialized MicroVM state.
func (m *Manager) BuildImage(ctx context.Context, in BuildImageInput) (*Image, error) {
	if in.Name == "" {
		return nil, errors.New("microvm: BuildImageInput.Name is required")
	}
	if in.FS == nil {
		return nil, errors.New("microvm: BuildImageInput.FS is required")
	}

	root := cleanRoot(in.Root)
	if _, err := fs.Stat(in.FS, path.Join(root, "Dockerfile")); err != nil {
		return nil, fmt.Errorf("microvm: Dockerfile must exist at %q: %w", path.Join(root, "Dockerfile"), err)
	}

	artifact, err := zipFS(in.FS, root, in.Exclude)
	if err != nil {
		return nil, err
	}

	bucket := firstNonEmpty(in.ArtifactBucket, m.bucket)
	if bucket == "" {
		return nil, errors.New("microvm: artifact bucket is required")
	}

	key := in.ArtifactKey
	if key == "" {
		key = defaultArtifactKey(m.artifactPrefix, in.Name)
	}

	if _, err := m.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        ptr(bucket),
		Key:           ptr(key),
		Body:          bytes.NewReader(artifact),
		ContentLength: ptr(int64(len(artifact))),
		ContentType:   ptr("application/zip"),
	}); err != nil {
		return nil, fmt.Errorf("microvm: upload artifact to s3://%s/%s: %w", bucket, key, err)
	}

	baseImageARN := firstNonEmpty(in.BaseImageARN, m.baseImageARN)
	if baseImageARN == "" {
		return nil, errors.New("microvm: base image ARN is required")
	}

	buildRoleARN := firstNonEmpty(in.BuildRoleARN, m.buildRoleARN)
	if buildRoleARN == "" {
		return nil, errors.New("microvm: build role ARN is required")
	}

	artifactURI := "s3://" + bucket + "/" + key
	createInput := &lambdamicrovms.CreateMicrovmImageInput{
		Name:                     ptr(in.Name),
		CodeArtifact:             &types.CodeArtifactMemberUri{Value: artifactURI},
		BaseImageArn:             ptr(baseImageARN),
		BuildRoleArn:             ptr(buildRoleARN),
		Description:              optionalString(in.Description),
		EnvironmentVariables:     cloneMap(in.EnvironmentVariables),
		Tags:                     cloneMap(in.Tags),
		Hooks:                    in.Hooks,
		Logging:                  in.Logging,
		EgressNetworkConnectors:  append([]string(nil), in.EgressNetworkConnectors...),
		AdditionalOsCapabilities: append([]types.Capability(nil), in.AdditionalOsCapabilities...),
	}
	if in.ResourcesMiB > 0 {
		createInput.Resources = []types.Resources{{MinimumMemoryInMiB: ptr(in.ResourcesMiB)}}
	}
	if in.MutateCreateInput != nil {
		in.MutateCreateInput(createInput)
	}

	out, err := m.mvm.CreateMicrovmImage(ctx, createInput)
	if err != nil {
		return nil, fmt.Errorf("microvm: create image: %w", err)
	}

	image := imageFromCreateOutput(out, artifactURI)
	if !in.Wait {
		return image, nil
	}

	waited, err := m.WaitImageReady(ctx, WaitImageInput{
		ImageIdentifier: firstNonEmpty(image.ARN, in.Name),
		PollInterval:    in.WaitPollInterval,
	})
	if err != nil {
		return nil, err
	}
	waited.ArtifactURI = artifactURI
	return waited, nil
}

// WaitImageInput configures WaitImageReady polling.
type WaitImageInput struct {
	// ImageIdentifier is the MicroVM image name or ARN to pass to GetMicrovmImage.
	ImageIdentifier string
	// PollInterval is the delay between polling attempts. If zero, the Manager
	// default is used.
	PollInterval time.Duration
}

// WaitImageReady polls GetMicrovmImage until the image is ready or failed.
//
// CREATED and UPDATED are treated as ready states. CREATE_FAILED,
// UPDATE_FAILED, DELETE_FAILED, and DELETED are treated as terminal failures.
func (m *Manager) WaitImageReady(ctx context.Context, in WaitImageInput) (*Image, error) {
	if in.ImageIdentifier == "" {
		return nil, errors.New("microvm: image identifier is required")
	}
	interval := in.PollInterval
	if interval <= 0 {
		interval = m.pollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		out, err := m.mvm.GetMicrovmImage(ctx, &lambdamicrovms.GetMicrovmImageInput{
			ImageIdentifier: ptr(in.ImageIdentifier),
		})
		if err != nil {
			return nil, fmt.Errorf("microvm: get image %q: %w", in.ImageIdentifier, err)
		}

		image := imageFromGetOutput(out)
		switch out.State {
		case types.MicrovmImageStateCreated, types.MicrovmImageStateUpdated:
			return image, nil
		case types.MicrovmImageStateCreateFailed, types.MicrovmImageStateUpdateFailed, types.MicrovmImageStateDeleteFailed, types.MicrovmImageStateDeleted:
			return nil, fmt.Errorf("microvm: image %q reached terminal state %s", in.ImageIdentifier, out.State)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func zipFS(source fs.FS, root string, exclude func(string, fs.DirEntry) bool) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	err := fs.WalkDir(source, root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == root {
			return nil
		}
		if exclude != nil && exclude(name, entry) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("microvm: unsupported non-regular file %q", name)
		}

		zipName := strings.TrimPrefix(name, root+"/")
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = zipName
		header.Method = zip.Deflate

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		file, err := source.Open(name)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(writer, file)
		return err
	})
	if err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("microvm: create artifact zip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("microvm: close artifact zip: %w", err)
	}
	return buf.Bytes(), nil
}

func cleanRoot(root string) string {
	root = strings.Trim(root, "/")
	if root == "" || root == "." {
		return "."
	}
	return path.Clean(root)
}

func defaultArtifactKey(prefix, name string) string {
	key := fmt.Sprintf("%s-%d.zip", name, time.Now().UTC().UnixNano())
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

func imageFromCreateOutput(out *lambdamicrovms.CreateMicrovmImageOutput, artifactURI string) *Image {
	return &Image{
		ARN:         deref(out.ImageArn),
		Name:        deref(out.Name),
		Version:     deref(out.ImageVersion),
		State:       out.State,
		ArtifactURI: artifactURI,
	}
}

func imageFromGetOutput(out *lambdamicrovms.GetMicrovmImageOutput) *Image {
	return &Image{
		ARN:     deref(out.ImageArn),
		Name:    deref(out.Name),
		Version: deref(out.LatestActiveImageVersion),
		State:   out.State,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func optionalString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
