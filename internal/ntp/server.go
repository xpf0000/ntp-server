package ntp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/aykutsp/ntp-server/internal/config"
	"github.com/aykutsp/ntp-server/internal/metrics"
	"github.com/aykutsp/ntp-server/internal/policy"
	"github.com/aykutsp/ntp-server/internal/ratelimit"
)

type Status struct {
	ClockMode       config.ClockMode `json:"clockMode"`
	NowUTC          time.Time        `json:"nowUTC"`
	ListenAddress   string           `json:"listenAddress"`
	Workers         int              `json:"workers"`
	Synced          bool             `json:"synced"`
	Stratum         uint8            `json:"stratum"`
	OffsetMillis    float64          `json:"offsetMillis"`
	ReferenceID     string           `json:"referenceID"`
	ReferenceTime   time.Time        `json:"referenceTime"`
	Upstream        string           `json:"upstream"`
	LastSyncError   string           `json:"lastSyncError"`
	ClientRateLimit float64          `json:"clientRateLimitPerSecond"`
	GlobalRateLimit float64          `json:"globalRateLimitPerSecond"`
}

type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	metrics *metrics.Counters

	conn            *net.UDPConn
	aclPolicy       *policy.CIDRPolicy
	dnsPolicy       *policy.DNSPolicy
	clientLimiter   *ratelimit.KeyedLimiter
	globalLimiter   *ratelimit.TokenBucket
	syncState       *syncState
	clock           config.ClockSettings
	simulatedSource SyncSnapshot
	defaults        ResponseDefaults

	readTimeout  time.Duration
	writeTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewServer(cfg config.Config, logger *slog.Logger, m *metrics.Counters) (*Server, error) {
	clock, err := cfg.Clock.Resolve()
	if err != nil {
		return nil, err
	}
	addr, err := net.ResolveUDPAddr("udp", cfg.NTP.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve listen address %q: %w", cfg.NTP.ListenAddress, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %q: %w", cfg.NTP.ListenAddress, err)
	}
	if cfg.NTP.ReadBufferBytes > 0 {
		_ = conn.SetReadBuffer(cfg.NTP.ReadBufferBytes)
	}
	if cfg.NTP.WriteBufferBytes > 0 {
		_ = conn.SetWriteBuffer(cfg.NTP.WriteBufferBytes)
	}

	acl, err := policy.NewCIDRPolicy(cfg.Policy.AllowCIDRs, cfg.Policy.DenyCIDRs)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	dnsPolicy := policy.NewDNSPolicy(policy.DNSPolicyConfig{
		Enabled:            cfg.Policy.DNS.EnableReverseLookup,
		AllowSuffixes:      cfg.Policy.DNS.AllowSuffixes,
		DenySuffixes:       cfg.Policy.DNS.DenySuffixes,
		CacheTTL:           time.Duration(cfg.Policy.DNS.CacheTTLSeconds) * time.Second,
		LookupTimeout:      time.Duration(cfg.Policy.DNS.LookupTimeoutMillis) * time.Millisecond,
		AllowOnLookupError: cfg.Policy.DNS.AllowOnLookupError,
		Nameservers:        cfg.Policy.DNS.ResolverNameservers,
	})

	defaultRef := ReferenceID(cfg.NTP.ReferenceID)
	s := &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: m,

		conn:      conn,
		aclPolicy: acl,
		dnsPolicy: dnsPolicy,
		clientLimiter: ratelimit.NewKeyedLimiter(
			cfg.NTP.ClientRateLimitPerSecond,
			cfg.NTP.ClientRateLimitBurst,
			10*time.Minute,
			128,
		),
		globalLimiter: ratelimit.NewTokenBucket(cfg.NTP.GlobalRateLimitPerSecond, cfg.NTP.GlobalRateLimitBurst),
		syncState:     newSyncState(defaultRef),
		clock:         clock,
		defaults: ResponseDefaults{
			ServeUnsynced:    cfg.NTP.ServeUnsynced,
			UnsyncedStratum:  cfg.NTP.UnsyncedStratum,
			LocalReferenceID: defaultRef,
			PrecisionExp:     cfg.NTP.PrecisionExponent,
			RootDelay:        time.Duration(cfg.NTP.RootDelayMillis * float64(time.Millisecond)),
			RootDispersion:   time.Duration(cfg.NTP.RootDispersionMillis * float64(time.Millisecond)),
		},
		readTimeout:  time.Duration(cfg.NTP.ReadTimeoutMillis) * time.Millisecond,
		writeTimeout: time.Duration(cfg.NTP.WriteTimeoutMillis) * time.Millisecond,
	}
	if clock.Mode != config.ClockReal {
		// Publish the configured clock as a local test reference on the wire.
		s.simulatedSource = SyncSnapshot{
			Synced:        true,
			Stratum:       10,
			ReferenceID:   ReferenceID("SIMU"),
			ReferenceTime: s.servedTime(time.Now(), clock.Offset),
			Offset:        clock.Offset,
		}
	}
	return s, nil
}

func (s *Server) Start(parent context.Context) {
	s.ctx, s.cancel = context.WithCancel(parent)

	upstreamEnabled := s.clock.Mode == config.ClockReal && s.cfg.Upstream.Enabled
	if upstreamEnabled {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			runUpstreamSyncLoop(s.ctx, s.cfg.Upstream, s.syncState, s.metrics, s.logger)
		}()
	}

	for i := 0; i < s.cfg.NTP.Workers; i++ {
		s.wg.Add(1)
		go func(id int) {
			defer s.wg.Done()
			s.workerLoop(id)
		}(i + 1)
	}

	s.logger.Info("NTP server started",
		"listen", s.cfg.NTP.ListenAddress,
		"workers", s.cfg.NTP.Workers,
		"clock_mode", s.clock.Mode,
		"upstream_enabled", upstreamEnabled,
	)
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.wg.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) workerLoop(workerID int) {
	buf := make([]byte, 512)

	for {
		if s.ctx.Err() != nil {
			return
		}
		if s.readTimeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.readTimeout))
		}

		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if s.ctx.Err() != nil {
				return
			}
			s.logger.Debug("udp read error", "worker", workerID, "error", err.Error())
			continue
		}

		s.metrics.IncRequests()
		s.metrics.AddBytesIn(n)

		if n < packetLen || !IsClientRequest(buf[:n]) {
			s.metrics.IncMalformed()
			continue
		}

		request := make([]byte, packetLen)
		copy(request, buf[:packetLen])
		receivedAt := time.Now().UTC()

		if !s.aclPolicy.Allow(addr.IP) {
			s.metrics.IncACLDenied()
			s.sendKissOfDeath(addr, request, receivedAt)
			continue
		}

		if ok, reason := s.dnsPolicy.Allow(s.ctx, addr.IP); !ok {
			s.metrics.IncDNSDenied()
			s.logger.Debug("request denied by reverse DNS policy",
				"client", addr.IP.String(),
				"reason", reason,
			)
			s.sendKissOfDeath(addr, request, receivedAt)
			continue
		}

		if !s.globalLimiter.Allow(receivedAt) || !s.clientLimiter.Allow(addr.IP.String(), receivedAt) {
			s.metrics.IncRateDenied()
			s.sendKissOfDeath(addr, request, receivedAt)
			continue
		}

		snapshot := s.clockSnapshot()
		if !snapshot.Synced && !s.cfg.NTP.ServeUnsynced {
			continue
		}

		transmittedAt := s.servedTime(time.Now(), snapshot.Offset)
		response := BuildResponse(
			request,
			s.servedTime(receivedAt, snapshot.Offset),
			transmittedAt,
			snapshot,
			s.defaults,
		)

		if s.writeTimeout > 0 {
			_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
		}

		written, err := s.conn.WriteToUDP(response[:], addr)
		if err != nil {
			s.logger.Debug("udp write error", "client", addr.String(), "error", err.Error())
			continue
		}
		s.metrics.IncResponses()
		s.metrics.AddBytesOut(written)
		if s.cfg.NTP.LogRequests {
			s.logger.Info("NTP request answered", "client", addr.String(), "clock_mode", s.clock.Mode, "served_time", transmittedAt)
		}
	}
}

func (s *Server) sendKissOfDeath(addr *net.UDPAddr, req []byte, now time.Time) {
	if !s.cfg.NTP.EnableKissOfDeathOnDeny || len(req) < packetLen {
		return
	}

	code := strings.TrimSpace(s.cfg.NTP.KissOfDeathResponseOnDeny)
	if code == "" {
		code = "RATE"
	}
	served := s.servedTime(now, s.clock.Offset)
	kod := BuildKissOfDeath(req, served, served, code)

	if s.writeTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	}
	if written, err := s.conn.WriteToUDP(kod[:], addr); err == nil {
		s.metrics.IncKissOfDeath()
		s.metrics.AddBytesOut(written)
	}
}

func (s *Server) clockSnapshot() SyncSnapshot {
	if s.clock.Mode == config.ClockReal {
		return s.syncState.Snapshot()
	}
	return s.simulatedSource
}

func (s *Server) servedTime(now time.Time, offset time.Duration) time.Time {
	if s.clock.Mode == config.ClockFrozen {
		return s.clock.FrozenTime
	}
	return now.UTC().Add(offset)
}

func (s *Server) Status() Status {
	ss := s.clockSnapshot()
	now := time.Now().UTC()
	served := s.servedTime(now, ss.Offset)
	return Status{
		ClockMode:       s.clock.Mode,
		NowUTC:          served,
		ListenAddress:   s.cfg.NTP.ListenAddress,
		Workers:         s.cfg.NTP.Workers,
		Synced:          s.clock.Mode == config.ClockReal && ss.Synced,
		Stratum:         s.effectiveStratum(ss),
		OffsetMillis:    durationMillis(served.Sub(now)),
		ReferenceID:     strings.TrimSpace(string(ss.ReferenceID[:])),
		ReferenceTime:   ss.ReferenceTime,
		Upstream:        ss.Upstream,
		LastSyncError:   ss.LastError,
		ClientRateLimit: s.cfg.NTP.ClientRateLimitPerSecond,
		GlobalRateLimit: s.cfg.NTP.GlobalRateLimitPerSecond,
	}
}

func (s *Server) effectiveStratum(ss SyncSnapshot) uint8 {
	if ss.Synced {
		if ss.Stratum == 0 {
			return 1
		}
		return ss.Stratum
	}
	return s.cfg.NTP.UnsyncedStratum
}

func (s *Server) Config() config.Config {
	return s.cfg
}
