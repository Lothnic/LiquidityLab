// Package inference accepts semantic policy proposals. It has no venue transport
// and currently permits only explicitly configured paper or shadow operation.
package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	ErrInvalid      = errors.New("invalid inference configuration or request")
	ErrBusy         = errors.New("policy worker admission full")
	ErrIncompatible = errors.New("incompatible policy worker response")
	ErrStale        = errors.New("policy proposal no longer applies")
)

const MaxValidity = 200 * time.Millisecond

type Config struct {
	Target           string
	PolicySHA256     string
	Profile          string
	AllowUnvalidated bool
	Timeout          time.Duration
}

type Client struct {
	connection *grpc.ClientConn
	rpc        pb.PolicyServiceClient
	config     Config
	status     string
	slot       chan struct{}
}

func SchemaSHA256() string {
	// encoding/json sorts map keys. These schema fields are ASCII and therefore
	// have identical encoding to the Python canonical JSON contract.
	encoded, _ := json.Marshal(map[string]any{"fields": x.ObservationFields, "version": x.ObservationVersion})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validDigest(s string) bool {
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == sha256.Size && s == strings.ToLower(s)
}

func localTarget(target string) (string, error) {
	if strings.HasPrefix(target, "unix:") {
		path := strings.TrimPrefix(target, "unix:")
		if !filepath.IsAbs(path) || len(path) > 103 || path == "/" || strings.ContainsRune(path, 0) {
			return "", ErrInvalid
		}
		return "unix:" + path, nil
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return "", ErrInvalid
	}
	if port == "" {
		return "", ErrInvalid
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", ErrInvalid
		}
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return "", ErrInvalid
	}
	return "passthrough:///" + target, nil
}

// New verifies the loaded worker identity. No manifest flag enables live mode;
// evaluated policy promotion is a separate, as yet unimplemented authority.
func New(ctx context.Context, config Config) (*Client, error) {
	if (config.Profile != "paper" && config.Profile != "shadow") || !config.AllowUnvalidated || !validDigest(config.PolicySHA256) {
		return nil, ErrInvalid
	}
	if config.Timeout == 0 {
		config.Timeout = 100 * time.Millisecond
	}
	if config.Timeout <= 0 || config.Timeout > MaxValidity {
		return nil, ErrInvalid
	}
	target, err := localTarget(config.Target)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4096), grpc.MaxCallSendMsgSize(8192)))
	if err != nil {
		return nil, err
	}
	client := &Client{connection: conn, rpc: pb.NewPolicyServiceClient(conn), config: config, slot: make(chan struct{}, 1)}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	info, err := client.rpc.GetPolicyInfo(checkCtx, &pb.PolicyInfoRequest{PolicySha256: config.PolicySHA256})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if info.PolicySha256 != config.PolicySHA256 || info.ObservationVersion != x.ObservationVersion || !slices.Equal(info.FieldNames, x.ObservationFields) || info.LiveEligible || info.PromotionStatus == "" || info.PromotionStatus == "live" || len(info.PromotionStatus) > 64 {
		conn.Close()
		return nil, ErrIncompatible
	}
	client.status = info.PromotionStatus
	return client, nil
}

// PromotionStatus is the immutable, verified worker status pinned at connection.
// It reports provenance and grants no live authority.
func (c *Client) PromotionStatus() string { return c.status }

func (c *Client) Close() error { return c.connection.Close() }

type Proposal struct {
	Binding *pb.DecisionBinding
	Action  d.Action
	Mode    string
}

func (c *Client) Propose(ctx context.Context, request *pb.DecisionRequest) (Proposal, error) {
	if request == nil || request.Binding == nil || request.Observation == nil || proto.Size(request) > 8192 {
		return Proposal{}, ErrInvalid
	}
	// Copy before RPC so the response is checked against the submitted binding.
	input := proto.Clone(request).(*pb.DecisionRequest)
	b := input.Binding
	for _, value := range []string{b.RequestId, b.TenantId, b.AccountId, b.SessionId, b.MarketSnapshotId} {
		if len(value) == 0 || len(value) > 256 {
			return Proposal{}, ErrInvalid
		}
	}
	if b.SessionRevision == 0 || b.AccountRevision == 0 || b.PolicySha256 != c.config.PolicySHA256 || b.ObservationVersion != x.ObservationVersion || b.ObservationSchemaSha256 != SchemaSHA256() || input.Observation.SchemaVersion != x.ObservationVersion || !slices.Equal(input.Observation.FieldNames, x.ObservationFields) || len(input.Observation.Features) != x.ObservationSize {
		return Proposal{}, ErrInvalid
	}
	for _, f := range input.Observation.Features {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return Proposal{}, ErrInvalid
		}
	}
	now := time.Now()
	expires := time.Unix(0, b.ExpiresAtUnixNs)
	if !expires.After(now) || expires.Sub(now) > MaxValidity {
		return Proposal{}, ErrStale
	}
	select {
	case c.slot <- struct{}{}:
		defer func() { <-c.slot }()
	default:
		return Proposal{}, ErrBusy
	}
	deadline := now.Add(c.config.Timeout)
	if expires.Before(deadline) {
		deadline = expires
	}
	rpcCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	response, err := c.rpc.Predict(rpcCtx, input)
	if err != nil {
		// Preserve the transport status while exposing stable causes to runtime
		// fallback classification. Worker messages never become durable reasons.
		var cause error
		switch status.Code(err) {
		case codes.ResourceExhausted:
			cause = ErrBusy
		case codes.DeadlineExceeded:
			cause = context.DeadlineExceeded
		case codes.Canceled:
			cause = context.Canceled
		case codes.InvalidArgument:
			cause = ErrInvalid
		}
		if cause != nil {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		return Proposal{}, fmt.Errorf("policy inference unavailable: %w", err)
	}
	if err := rpcCtx.Err(); err != nil || !time.Now().Before(expires) {
		return Proposal{}, ErrStale
	}
	if response == nil || !proto.Equal(response.Binding, b) || response.Mode != c.status || d.Action(response.Action).Validate() != nil {
		return Proposal{}, ErrIncompatible
	}
	return Proposal{Binding: proto.Clone(b).(*pb.DecisionBinding), Action: d.Action(response.Action), Mode: response.Mode}, nil
}

type Current struct {
	TenantID, AccountID, SessionID   string
	SessionRevision, AccountRevision uint64
	PolicySHA256                     string
	Active, Ready, MarketValid       bool
}

// ValidateCurrent runs under the account authority immediately before compiling
// a proposal. New market snapshots are allowed; state or plan changes are not.
func (p Proposal) ValidateCurrent(current Current, now time.Time) error {
	b := p.Binding
	if b == nil || p.Action.Validate() != nil || !current.Active || !current.Ready || !current.MarketValid || !now.Before(time.Unix(0, b.ExpiresAtUnixNs)) || b.TenantId != current.TenantID || b.AccountId != current.AccountID || b.SessionId != current.SessionID || b.SessionRevision != current.SessionRevision || b.AccountRevision != current.AccountRevision || b.PolicySha256 != current.PolicySHA256 || b.ObservationVersion != x.ObservationVersion || b.ObservationSchemaSha256 != SchemaSHA256() {
		return ErrStale
	}
	return nil
}
