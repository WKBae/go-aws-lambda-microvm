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

// RunInput describes a MicroVM launch from an existing MicroVM image.
type RunInput struct {
	// ImageIdentifier is the ARN or identifier of the MicroVM image to run. This
	// field is required and maps to RunMicrovmInput.ImageIdentifier.
	ImageIdentifier string
	// ImageVersion selects a specific image version. If empty, Lambda uses the
	// latest active version of the image.
	ImageVersion string

	// ExecutionRoleARN is the IAM role assumed by the MicroVM at runtime. Use it
	// when the application or lifecycle hooks need AWS permissions.
	ExecutionRoleARN string
	// RunHookPayload is per-MicroVM initialization data delivered to the /run
	// lifecycle hook. The service limit is 16 KiB.
	RunHookPayload string
	// IdlePolicy configures automatic suspend and resume behavior based on
	// inbound endpoint traffic.
	IdlePolicy *types.IdlePolicy
	// Logging configures runtime CloudWatch Logs output, or disables logging.
	// Leave nil to use the service default.
	Logging types.Logging

	// IngressNetworkConnectors lists connector ARNs that enable inbound HTTPS
	// connectivity. If empty, Manager.Config.DefaultIngressConnectors is used.
	IngressNetworkConnectors []string
	// EgressNetworkConnectors lists connector ARNs for outbound connectivity,
	// such as public internet or VPC egress. If empty, the Manager defaults are
	// used.
	EgressNetworkConnectors []string

	// MaximumDuration is the maximum time the MicroVM may exist in running or
	// suspended states before Lambda terminates it. Lambda supports up to 8 hours.
	MaximumDuration time.Duration

	// AllowedPorts scopes the endpoint auth token created for the returned
	// Session. If empty, the token is scoped to DefaultMicroVMPort.
	AllowedPorts []types.PortSpecification
	// TokenExpiration controls the auth token lifetime for endpoint requests.
	// Lambda currently limits MicroVM auth tokens to 60 minutes; larger values
	// are clamped to one hour.
	TokenExpiration time.Duration
	// Wait tells Run to poll GetMicrovm until the MicroVM reaches RUNNING before
	// creating the endpoint auth token and returning the Session.
	Wait bool
	// WaitPollInterval overrides the Manager polling interval for this run.
	WaitPollInterval time.Duration
	// MutateRunInput can adjust the generated AWS SDK input immediately before
	// RunMicrovm is called.
	MutateRunInput func(*lambdamicrovms.RunMicrovmInput)
	// MutateTokenInput can adjust the generated CreateMicrovmAuthToken input
	// before each token creation or refresh.
	MutateTokenInput func(*lambdamicrovms.CreateMicrovmAuthTokenInput)
}

// MicroVM summarizes a running or queried MicroVM instance.
type MicroVM struct {
	// ID is the service-generated MicroVM identifier, such as mvm-...
	ID string
	// Endpoint is the HTTPS endpoint host used to communicate with this MicroVM.
	// Requests must include a valid X-aws-proxy-auth token.
	Endpoint string
	// ImageARN is the ARN of the MicroVM image used to launch the instance.
	ImageARN string
	// Version is the MicroVM image version used to launch the instance.
	Version string
	// State is the current MicroVM lifecycle state.
	State types.MicrovmState
	// StartedAt is the timestamp reported by Lambda when the MicroVM first
	// started. It is zero when the service response omits the timestamp.
	StartedAt time.Time
}

// Session represents a MicroVM plus an endpoint auth token.
//
// A Session is returned by Run and provides convenience methods for sending
// authenticated HTTP requests to the MicroVM endpoint. It automatically includes
// X-aws-proxy-auth and refreshes the token shortly before expiration.
type Session struct {
	manager *Manager

	// ID is the service-generated MicroVM identifier.
	ID string
	// Endpoint is the HTTPS endpoint host used for application traffic.
	Endpoint string

	allowedPorts     []types.PortSpecification
	tokenExpiration  time.Duration
	mutateTokenInput func(*lambdamicrovms.CreateMicrovmAuthTokenInput)

	token          string
	tokenExpiresAt time.Time
}

// Run launches a MicroVM, creates an endpoint auth token, and returns a Session.
//
// The returned Session can call the MicroVM's dedicated HTTPS endpoint. If
// RunInput.Wait is true, Run waits for the MicroVM to reach RUNNING before
// creating the token.
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

// WaitMicroVMInput configures WaitMicroVMRunning polling.
type WaitMicroVMInput struct {
	// MicroVMIdentifier is the MicroVM ID to pass to GetMicrovm.
	MicroVMIdentifier string
	// PollInterval is the delay between polling attempts. If zero, the Manager
	// default is used.
	PollInterval time.Duration
}

// WaitMicroVMRunning polls GetMicrovm until the MicroVM reaches RUNNING.
//
// TERMINATING and TERMINATED are treated as terminal failures.
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

// Do sends req to the MicroVM endpoint with Lambda MicroVM proxy headers.
//
// If req.URL lacks a scheme or host, Do fills in https and the Session endpoint.
// The port argument sets X-aws-proxy-port; pass 0 to omit the header and let
// Lambda route to the default port 8080.
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

// NewRequest creates an HTTP request targeting the MicroVM endpoint.
//
// If target is a relative path, it is resolved against https://Session.Endpoint.
// Absolute http:// or https:// URLs are preserved.
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

// Get sends an authenticated HTTP GET request to the MicroVM endpoint.
//
// The target may be a relative path such as "/health" or an absolute URL. The
// port argument is forwarded as X-aws-proxy-port when greater than zero.
func (s *Session) Get(ctx context.Context, target string, port int32) (*http.Response, error) {
	req, err := s.NewRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return s.Do(req, port)
}

// Suspend suspends the MicroVM while preserving memory and disk state.
//
// Lambda invokes the application's /suspend lifecycle hook, if configured,
// before checkpointing the MicroVM.
func (s *Session) Suspend(ctx context.Context) error {
	_, err := s.manager.mvm.SuspendMicrovm(ctx, &lambdamicrovms.SuspendMicrovmInput{
		MicrovmIdentifier: ptr(s.ID),
	})
	if err != nil {
		return fmt.Errorf("microvm: suspend %q: %w", s.ID, err)
	}
	return nil
}

// Resume resumes a suspended MicroVM.
//
// Lambda restores preserved memory and disk state, invokes the /resume lifecycle
// hook if configured, and transitions the MicroVM back to RUNNING on success.
func (s *Session) Resume(ctx context.Context) error {
	_, err := s.manager.mvm.ResumeMicrovm(ctx, &lambdamicrovms.ResumeMicrovmInput{
		MicrovmIdentifier: ptr(s.ID),
	})
	if err != nil {
		return fmt.Errorf("microvm: resume %q: %w", s.ID, err)
	}
	return nil
}

// Terminate terminates the MicroVM and releases its resources.
//
// Lambda invokes the /terminate lifecycle hook, if configured, before releasing
// resources. A terminated MicroVM cannot be resumed.
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
