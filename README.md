# go-aws-lambda-microvm

[![Go Reference](https://pkg.go.dev/badge/github.com/WKBae/go-aws-lambda-microvm.svg)](https://pkg.go.dev/github.com/WKBae/go-aws-lambda-microvm)

`go-aws-lambda-microvm` is a small Go convenience library for AWS Lambda
MicroVMs. It wraps the common workflow of packaging an application filesystem,
uploading the artifact to S3, creating a MicroVM image, running a MicroVM, and
sending authenticated HTTP requests to the MicroVM endpoint.

The library keeps the AWS SDK for Go v2 as the control-plane implementation and
adds higher-level helpers around the workflows most applications need.

## Installation

```sh
go get github.com/WKBae/go-aws-lambda-microvm
```

## Usage

Create a manager from an AWS SDK config:

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/WKBae/go-aws-lambda-microvm"
	"github.com/aws/aws-sdk-go-v2/config"
)

func main() {
	ctx := context.Background()

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		log.Fatal(err)
	}

	manager, err := microvm.NewManager(microvm.Config{
		AWSConfig:      awsCfg,
		ArtifactBucket: "my-microvm-artifacts",
		BaseImageARN:   "arn:aws:lambda:us-east-1:aws:microvm-image:al2023-1",
		BuildRoleARN:   "arn:aws:iam::123456789012:role/MicrovmBuildRole",
		DefaultIngressConnectors: []string{
			microvm.AllIngressConnectorARN("us-east-1"),
		},
		DefaultEgressConnectors: []string{
			microvm.InternetEgressConnectorARN("us-east-1"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	image, err := manager.BuildImageFromDir(ctx, microvm.BuildImageFromDirInput{
		Name: "example-sandbox",
		Dir:  "./microvm-app",
		Wait: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	session, err := manager.Run(ctx, microvm.RunInput{
		ImageIdentifier: image.ARN,
		RunHookPayload:  `{"tenant":"tenant-123"}`,
		MaximumDuration: 4 * time.Hour,
		Wait:            true,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer session.Terminate(ctx)

	resp, err := session.Get(ctx, "/health", microvm.DefaultMicroVMPort)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
}
```

## Building Images from `fs.FS`

`BuildImage` accepts any `fs.FS`, which makes it suitable for `embed.FS`, test
filesystems, and generated in-memory filesystems. The selected root must contain
a `Dockerfile` at its top level.

```go
//go:embed microvm-app/*
var appFS embed.FS

image, err := manager.BuildImage(ctx, microvm.BuildImageInput{
	Name: "embedded-sandbox",
	FS:   appFS,
	Root: "microvm-app",
	Wait: true,
})
```

Use `BuildImageFromDir` when the artifact source is a local directory.

## Endpoint Requests

Every request to a Lambda MicroVM endpoint requires an auth token. `Run` creates
the first token and `Session` refreshes it before expiration. `Session.Do` and
`Session.Get` add the Lambda proxy headers for you:

- `X-aws-proxy-auth`
- `X-aws-proxy-port` when a port is supplied

Pass port `0` to omit `X-aws-proxy-port` and let Lambda route to the default
port, `8080`.

## Testing

```sh
go test ./...
go vet ./...
```

## Status

This module targets the AWS SDK for Go v2 Lambda MicroVMs client:

```go
github.com/aws/aws-sdk-go-v2/service/lambdamicrovms
```

The API is intentionally focused on convenience workflows rather than exposing
every Lambda MicroVMs operation. Advanced callers can use the `Mutate*Input`
hooks to customize the generated AWS SDK inputs.
