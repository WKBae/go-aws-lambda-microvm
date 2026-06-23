package microvm

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	DefaultMicroVMPort = int32(8080)
)

func AllIngressConnectorARN(region string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:ALL_INGRESS", region)
}

func InternetEgressConnectorARN(region string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:INTERNET_EGRESS", region)
}

func NoIngressConnectorARN(region string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:NO_INGRESS", region)
}

func ShellIngressConnectorARN(region string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:SHELL_INGRESS", region)
}

type Manager struct {
	s3  s3API
	mvm microvmsAPI

	httpClient *http.Client

	bucket         string
	artifactPrefix string
	baseImageARN   string
	buildRoleARN   string

	defaultIngress []string
	defaultEgress  []string
	pollInterval   time.Duration
}

type Config struct {
	AWSConfig aws.Config

	S3Client      *s3.Client
	MicroVMClient *lambdamicrovms.Client
	HTTPClient    *http.Client

	ArtifactBucket string
	ArtifactPrefix string
	BaseImageARN   string
	BuildRoleARN   string

	DefaultIngressConnectors []string
	DefaultEgressConnectors  []string
	PollInterval             time.Duration
}

func NewManager(cfg Config) (*Manager, error) {
	s3Client := cfg.S3Client
	if s3Client == nil {
		if cfg.AWSConfig.Region == "" {
			return nil, errors.New("microvm: AWSConfig.Region is required when S3Client is not provided")
		}
		s3Client = s3.NewFromConfig(cfg.AWSConfig)
	}

	microvmClient := cfg.MicroVMClient
	if microvmClient == nil {
		if cfg.AWSConfig.Region == "" {
			return nil, errors.New("microvm: AWSConfig.Region is required when MicroVMClient is not provided")
		}
		microvmClient = lambdamicrovms.NewFromConfig(cfg.AWSConfig)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}

	return &Manager{
		s3:             s3Client,
		mvm:            microvmClient,
		httpClient:     httpClient,
		bucket:         cfg.ArtifactBucket,
		artifactPrefix: cfg.ArtifactPrefix,
		baseImageARN:   cfg.BaseImageARN,
		buildRoleARN:   cfg.BuildRoleARN,
		defaultIngress: append([]string(nil), cfg.DefaultIngressConnectors...),
		defaultEgress:  append([]string(nil), cfg.DefaultEgressConnectors...),
		pollInterval:   pollInterval,
	}, nil
}
