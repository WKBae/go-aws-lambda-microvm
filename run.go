package microvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
)

const authHeader = "X-aws-proxy-auth"
const portHeader = "X-aws-proxy-port"

type RunInput struct {
	ImageIdentifier string
	ImageVersion    string

	ExecutionRoleARN string
	RunHookPayload   string
	IdlePolicy       *types.IdlePolicy
	Logging          types.Logging

	IngressNetworkConnectors []string
	EgressNetworkConnectors  []string

	MaximumDuration time.Duration

	AllowedPorts     []types.PortSpecification
	TokenExpiration  time.Duration
	Wait             bool
	WaitPollInterval time.Duration
	MutateRunInput   func(*lambdamicrovms.RunMicrovmInput)
	MutateTokenInput func(*lambdamicrovms.CreateMicrovmAuthTokenInput)
}

type MicroVM struct {
	ID        string
	Endpoint  string
	ImageARN  string
	Version   string
	State     types.MicrovmState
	StartedAt time.Time
}

type Session struct {
	manager *Manager

	ID       string
	Endpoint string

	allowedPorts     []types.PortSpecification
	tokenExpiration  time.Duration
	mutateTokenInput func(*lambdamicrovms.CreateMicrovmAuthTokenInput)

	token          string
	tokenExpiresAt time.Time
}

func (m *Manager) Run(ctx context.Context, in RunInput) (*Session, error) {
	if in.ImageIdentifier == "" {
		return nil, errors.New("microvm: RunInput.ImageIdentifier is required")
	}

	runInput := &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier:          ptr(in.ImageIdentifier),
		ImageVersion:             optionalString(in.ImageVersion),
		ExecutionRoleArn:         optionalString(in.ExecutionRoleARN),
		RunHookPayload:           optionalString(in.RunHookPayload),
		IdlePolicy:               in.IdlePolicy,
		Logging:                  in.Logging,
		IngressNetworkConnectors: connectorsOrDefault(in.IngressNetworkConnectors, m.defaultIngress),
		EgressNetworkConnectors:  connectorsOrDefault(in.EgressNetworkConnectors, m.defaultEgress),
	}
	if in.MaximumDuration > 0 {
		runInput.MaximumDurationInSeconds = ptr(int32(in.MaximumDuration / time.Second))
	}
	if in.MutateRunInput != nil {
		in.MutateRunInput(runInput)
	}

	out, err := m.mvm.RunMicrovm(ctx, runInput)
	if err != nil {
		return nil, fmt.Errorf("microvm: run MicroVM: %w", err)
	}

	microvmID := deref(out.MicrovmId)
	endpoint := deref(out.Endpoint)
	if in.Wait {
		vm, err := m.WaitMicroVMRunning(ctx, WaitMicroVMInput{
			MicroVMIdentifier: microvmID,
			PollInterval:      in.WaitPollInterval,
		})
		if err != nil {
			return nil, err
		}
		if vm.Endpoint != "" {
			endpoint = vm.Endpoint
		}
	}

	session := &Session{
		manager:          m,
		ID:               microvmID,
		Endpoint:         normalizeEndpointHost(endpoint),
		allowedPorts:     defaultAllowedPorts(in.AllowedPorts),
		tokenExpiration:  defaultTokenExpiration(in.TokenExpiration),
		mutateTokenInput: in.MutateTokenInput,
	}
	if err := session.refreshToken(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

type WaitMicroVMInput struct {
	MicroVMIdentifier string
	PollInterval      time.Duration
}

func (m *Manager) WaitMicroVMRunning(ctx context.Context, in WaitMicroVMInput) (*MicroVM, error) {
	if in.MicroVMIdentifier == "" {
		return nil, errors.New("microvm: MicroVM identifier is required")
	}
	interval := in.PollInterval
	if interval <= 0 {
		interval = m.pollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		out, err := m.mvm.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{
			MicrovmIdentifier: ptr(in.MicroVMIdentifier),
		})
		if err != nil {
			return nil, fmt.Errorf("microvm: get MicroVM %q: %w", in.MicroVMIdentifier, err)
		}

		vm := microVMFromGetOutput(out)
		switch out.State {
		case types.MicrovmStateRunning:
			return vm, nil
		case types.MicrovmStateTerminating, types.MicrovmStateTerminated:
			return nil, fmt.Errorf("microvm: MicroVM %q reached terminal state %s", in.MicroVMIdentifier, out.State)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Session) Do(req *http.Request, port int32) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("microvm: request is nil")
	}
	if err := s.ensureToken(req.Context()); err != nil {
		return nil, err
	}
	if req.URL.Scheme == "" {
		req.URL.Scheme = "https"
	}
	if req.URL.Host == "" {
		req.URL.Host = s.Endpoint
	}
	req.Header.Set(authHeader, s.token)
	if port > 0 {
		req.Header.Set(portHeader, fmt.Sprintf("%d", port))
	}
	return s.manager.httpClient.Do(req)
}

func (s *Session) NewRequest(ctx context.Context, method, target string, body io.Reader) (*http.Request, error) {
	url := target
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		if !strings.HasPrefix(url, "/") {
			url = "/" + url
		}
		url = "https://" + s.Endpoint + url
	}
	return http.NewRequestWithContext(ctx, method, url, body)
}

func (s *Session) Get(ctx context.Context, target string, port int32) (*http.Response, error) {
	req, err := s.NewRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return s.Do(req, port)
}

func (s *Session) Suspend(ctx context.Context) error {
	_, err := s.manager.mvm.SuspendMicrovm(ctx, &lambdamicrovms.SuspendMicrovmInput{
		MicrovmIdentifier: ptr(s.ID),
	})
	if err != nil {
		return fmt.Errorf("microvm: suspend %q: %w", s.ID, err)
	}
	return nil
}

func (s *Session) Resume(ctx context.Context) error {
	_, err := s.manager.mvm.ResumeMicrovm(ctx, &lambdamicrovms.ResumeMicrovmInput{
		MicrovmIdentifier: ptr(s.ID),
	})
	if err != nil {
		return fmt.Errorf("microvm: resume %q: %w", s.ID, err)
	}
	return nil
}

func (s *Session) Terminate(ctx context.Context) error {
	_, err := s.manager.mvm.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{
		MicrovmIdentifier: ptr(s.ID),
	})
	if err != nil {
		return fmt.Errorf("microvm: terminate %q: %w", s.ID, err)
	}
	return nil
}

func (s *Session) ensureToken(ctx context.Context) error {
	if s.token != "" && time.Until(s.tokenExpiresAt) > time.Minute {
		return nil
	}
	return s.refreshToken(ctx)
}

func (s *Session) refreshToken(ctx context.Context) error {
	minutes := int32(s.tokenExpiration / time.Minute)
	if minutes <= 0 {
		minutes = 30
	}
	input := &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   ptr(s.ID),
		ExpirationInMinutes: ptr(minutes),
		AllowedPorts:        append([]types.PortSpecification(nil), s.allowedPorts...),
	}
	if s.mutateTokenInput != nil {
		s.mutateTokenInput(input)
	}
	out, err := s.manager.mvm.CreateMicrovmAuthToken(ctx, input)
	if err != nil {
		return fmt.Errorf("microvm: create auth token for %q: %w", s.ID, err)
	}
	token := out.AuthToken[authHeader]
	if token == "" {
		return fmt.Errorf("microvm: auth token response missing %q", authHeader)
	}
	s.token = token
	s.tokenExpiresAt = time.Now().Add(time.Duration(minutes) * time.Minute)
	return nil
}

func normalizeEndpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
}

func connectorsOrDefault(value, fallback []string) []string {
	if len(value) > 0 {
		return append([]string(nil), value...)
	}
	return append([]string(nil), fallback...)
}

func defaultAllowedPorts(ports []types.PortSpecification) []types.PortSpecification {
	if len(ports) > 0 {
		return append([]types.PortSpecification(nil), ports...)
	}
	return []types.PortSpecification{&types.PortSpecificationMemberPort{Value: DefaultMicroVMPort}}
}

func defaultTokenExpiration(d time.Duration) time.Duration {
	if d <= 0 {
		return 30 * time.Minute
	}
	if d > time.Hour {
		return time.Hour
	}
	return d
}

func microVMFromGetOutput(out *lambdamicrovms.GetMicrovmOutput) *MicroVM {
	var startedAt time.Time
	if out.StartedAt != nil {
		startedAt = *out.StartedAt
	}
	return &MicroVM{
		ID:        deref(out.MicrovmId),
		Endpoint:  deref(out.Endpoint),
		ImageARN:  deref(out.ImageArn),
		Version:   deref(out.ImageVersion),
		State:     out.State,
		StartedAt: startedAt,
	}
}
