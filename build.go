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

type BuildImageInput struct {
	Name string
	FS   fs.FS
	Root string

	ArtifactBucket string
	ArtifactKey    string

	BaseImageARN string
	BuildRoleARN string

	Description              string
	EnvironmentVariables     map[string]string
	Tags                     map[string]string
	Hooks                    *types.Hooks
	Logging                  types.Logging
	ResourcesMiB             int32
	AdditionalOsCapabilities []types.Capability
	EgressNetworkConnectors  []string

	Exclude func(name string, entry fs.DirEntry) bool

	Wait             bool
	WaitPollInterval time.Duration

	MutateCreateInput func(*lambdamicrovms.CreateMicrovmImageInput)
}

type BuildImageFromDirInput struct {
	Name string
	Dir  string

	ArtifactBucket string
	ArtifactKey    string

	BaseImageARN string
	BuildRoleARN string

	Description              string
	EnvironmentVariables     map[string]string
	Tags                     map[string]string
	Hooks                    *types.Hooks
	Logging                  types.Logging
	ResourcesMiB             int32
	AdditionalOsCapabilities []types.Capability
	EgressNetworkConnectors  []string

	Exclude func(name string, entry fs.DirEntry) bool

	Wait             bool
	WaitPollInterval time.Duration

	MutateCreateInput func(*lambdamicrovms.CreateMicrovmImageInput)
}

type Image struct {
	ARN         string
	Name        string
	Version     string
	State       types.MicrovmImageState
	ArtifactURI string
}

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

type WaitImageInput struct {
	ImageIdentifier string
	PollInterval    time.Duration
}

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
