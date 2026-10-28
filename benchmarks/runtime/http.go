package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/api"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const maxHTTPResponseBytes int64 = 1 << 20

var errReadRejected = errors.New("HTTP polling admission rejected")

type readResult struct {
	Status int
	Bytes  int64
}
type readFunc func(context.Context, input) (readResult, error)
type httpPoller struct {
	server                      *http.Server
	listener                    net.Listener
	client                      *http.Client
	transport                   *http.Transport
	baseURL, token, accountPath string
	done                        chan struct{}
	once                        sync.Once
	closeErr                    error
}

func startHTTPPoller(c config, key pg.Key, authority api.Authority, journal api.JournalReader, sessions api.SessionReader) (*httpPoller, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, errors.New("HTTP credential generation failed")
	}
	token := hex.EncodeToString(secret[:])
	handler, err := api.New(api.Config{Accounts: map[pg.Key]api.Authority{key: authority}, Credentials: []api.Credential{{Token: token, Scopes: []pg.Key{key}}}, Journal: journal, Sessions: sessions, MaxConcurrent: c.InFlight, RequestTimeout: c.HTTPTimeout})
	if err != nil {
		return nil, errors.New("HTTP polling API configuration failed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("HTTP polling listener failed")
	}
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: c.InFlight, MaxIdleConns: c.InFlight, MaxIdleConnsPerHost: c.InFlight, IdleConnTimeout: 5 * time.Second, ResponseHeaderTimeout: c.HTTPTimeout, DisableCompression: true}
	p := &httpPoller{listener: listener, transport: transport, token: token, baseURL: "http://" + listener.Addr().String(), accountPath: "/v1/accounts/" + key.TenantID + "/" + key.AccountID, done: make(chan struct{})}
	p.client = &http.Client{Transport: transport, Timeout: c.HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p.server = &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() { defer close(p.done); _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *httpPoller) Read(ctx context.Context, in input) (readResult, error) {
	path := p.accountPath
	switch in.ReadRoute {
	case "state":
	case "sessions":
		path += "/sessions?limit=1"
	case "report":
		path += "/sessions/benchmark-plan"
	default:
		return readResult{}, errors.New("invalid HTTP read route")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return readResult{}, errors.New("HTTP polling request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return readResult{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return readResult{}, context.DeadlineExceeded
		}
		return readResult{}, errors.New("HTTP polling request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
	out := readResult{Status: response.StatusCode, Bytes: int64(len(data))}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return out, context.DeadlineExceeded
		}
		return out, errors.New("HTTP response consumption failed")
	}
	if int64(len(data)) > maxHTTPResponseBytes {
		return out, errors.New("HTTP response exceeded bounded size")
	}
	// Timing includes full body consumption and JSON syntax validation. No body,
	// bearer token, endpoint address, or raw network error is retained in reports.
	var object map[string]json.RawMessage
	if err = json.Unmarshal(data, &object); err != nil || object == nil {
		return out, errors.New("HTTP response JSON invalid")
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return out, errReadRejected
	}
	if response.StatusCode == http.StatusGatewayTimeout {
		return out, context.DeadlineExceeded
	}
	if response.StatusCode != http.StatusOK {
		return out, errors.New("HTTP polling status failed")
	}
	return out, nil
}

func (p *httpPoller) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		p.transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p.closeErr = p.server.Shutdown(ctx)
		if p.closeErr != nil {
			_ = p.server.Close()
		}
		<-p.done
	})
	return p.closeErr
}
