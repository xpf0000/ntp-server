package ntp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aykutsp/ntp-server/internal/config"
	"github.com/aykutsp/ntp-server/internal/metrics"
)

func startClockTestServer(t *testing.T, clockJSON string) *Server {
	t.Helper()
	return startClockTestServerWithLog(t, clockJSON, false, io.Discard)
}

func startClockTestServerWithLog(t *testing.T, clockJSON string, logRequests bool, output io.Writer) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfgJSON := `{
		"ntp": {"listenAddress": "127.0.0.1:0", "workers": 1, "serveUnsynced": false, "logRequests": ` + strconv.FormatBool(logRequests) + `},
		"upstream": {"enabled": false},
		"clock": ` + clockJSON + `
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(output, nil)), metrics.NewCounters())
	if err != nil {
		t.Fatal(err)
	}
	srv.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return srv
}

func queryClock(t *testing.T, srv *Server) []byte {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, srv.conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var request [packetLen]byte
	request[0] = 4<<3 | 3
	copy(request[40:], u64Bytes(ToTimestamp(time.Now())))
	if _, err := conn.Write(request[:]); err != nil {
		t.Fatal(err)
	}
	var response [packetLen]byte
	n, err := conn.Read(response[:])
	if err != nil || n != packetLen {
		t.Fatalf("expected a %d-byte UDP response, got %d bytes (%v)", packetLen, n, err)
	}
	if response[0]&7 != 4 || !bytes.Equal(response[24:32], request[40:48]) {
		t.Fatal("response did not identify this NTP request")
	}
	if response[0]>>6 != 0 || response[1] == 0 || response[1] >= 16 {
		t.Fatalf("invalid simulated time source metadata: LI=%d, stratum=%d", response[0]>>6, response[1])
	}
	return response[:]
}

func TestFrozenClockServesTheSameInstant(t *testing.T) {
	srv := startClockTestServer(t, `{"mode":"frozen","frozenTime":"2038-01-19T20:00:00+08:00"}`)
	want := time.Date(2038, 1, 19, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		response := queryClock(t, srv)
		for _, field := range []int{16, 32, 40} {
			if got := ReadTimestamp(response, field); !got.Equal(want) {
				t.Fatalf("timestamp at byte %d: got %v, want %v", field, got, want)
			}
		}
		status := srv.Status()
		if !status.NowUTC.Equal(want) || status.Synced {
			t.Fatalf("status should expose the frozen time without upstream synchronization: %+v", status)
		}
	}
}

func TestOffsetClockTicksWithPositiveAndNegativeOffsets(t *testing.T) {
	cases := []struct {
		name, input string
		offset      time.Duration
	}{
		{"positive", `{"mode":"offset","offsetSeconds":604800}`, 7 * 24 * time.Hour},
		{"negative", `{"mode":"offset","offsetSeconds":-7200}`, -2 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startClockTestServer(t, tc.input)
			before := time.Now().UTC()
			first := ReadTimestamp(queryClock(t, srv), 40)
			time.Sleep(20 * time.Millisecond)
			second := ReadTimestamp(queryClock(t, srv), 40)
			after := time.Now().UTC()
			if first.Before(before.Add(tc.offset-time.Millisecond)) || second.After(after.Add(tc.offset)) {
				t.Fatalf("served times %v and %v are outside the expected offset window", first, second)
			}
			if !second.After(first) {
				t.Fatalf("offset clock did not advance: %v then %v", first, second)
			}
			status := srv.Status()
			if status.OffsetMillis != float64(tc.offset/time.Millisecond) || status.Synced {
				t.Fatalf("unexpected offset status: %+v", status)
			}
		})
	}
}

func TestRequestLoggingUsesServedTime(t *testing.T) {
	frozen := `{"mode":"frozen","frozenTime":"2038-01-19T12:00:00Z"}`
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			var logs bytes.Buffer
			srv := startClockTestServerWithLog(t, frozen, enabled, &logs)
			queryClock(t, srv)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			got := logs.Bytes()
			logged := bytes.Contains(got, []byte("NTP request answered"))
			if logged != enabled || (enabled && (!bytes.Contains(got, []byte("2038-01-19T12:00:00")) || !bytes.Contains(got, []byte("127.0.0.1")))) {
				t.Fatalf("unexpected request log: %s", got)
			}
		})
	}
}
