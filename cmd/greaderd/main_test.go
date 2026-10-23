package main

import (
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{"GREADERD_USER": "u", "GREADERD_PASSWORD": "p"},
			want: config{addr: ":8080", dbPath: "greaderd.db", refreshInterval: 15 * time.Minute, retention: 90 * day},
		},
		{
			name: "overrides",
			env: map[string]string{
				"GREADERD_USER": "u", "GREADERD_PASSWORD": "p",
				"GREADERD_ADDR": "127.0.0.1:9000", "GREADERD_DB": "/tmp/x.db", "GREADERD_REFRESH_INTERVAL": "5m",
				"GREADERD_RETENTION": "0",
			},
			want: config{addr: "127.0.0.1:9000", dbPath: "/tmp/x.db", refreshInterval: 5 * time.Minute},
		},
		{name: "missing user", env: map[string]string{"GREADERD_PASSWORD": "p"}, wantErr: true},
		{name: "missing password", env: map[string]string{"GREADERD_USER": "u"}, wantErr: true},
		{
			name:    "invalid interval",
			env:     map[string]string{"GREADERD_USER": "u", "GREADERD_PASSWORD": "p", "GREADERD_REFRESH_INTERVAL": "soon"},
			wantErr: true,
		},
		{
			name:    "non-positive interval",
			env:     map[string]string{"GREADERD_USER": "u", "GREADERD_PASSWORD": "p", "GREADERD_REFRESH_INTERVAL": "0s"},
			wantErr: true,
		},
		{
			name: "flags",
			env: map[string]string{
				"GREADERD_USER": "u", "GREADERD_PASSWORD": "p",
				"GREADERD_TRUST_PROXY_HEADERS": "true", "GREADERD_ALLOW_PRIVATE_FEEDS": "1",
			},
			want: config{
				addr: ":8080", dbPath: "greaderd.db", refreshInterval: 15 * time.Minute, retention: 90 * day,
				trustProxyHeaders: true, allowPrivateFeeds: true,
			},
		},
		{
			name:    "invalid flag",
			env:     map[string]string{"GREADERD_USER": "u", "GREADERD_PASSWORD": "p", "GREADERD_TRUST_PROXY_HEADERS": "yes please"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("loadConfig = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			tt.want.creds = got.creds
			if got != tt.want {
				t.Errorf("loadConfig = %+v, want %+v", got, tt.want)
			}
			if got.creds.User != "u" || got.creds.Password != "p" {
				t.Errorf("credentials = %+v", got.creds)
			}
		})
	}
}

const day = 24 * time.Hour

func TestParseRetention(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "90d", want: 90 * day},
		{in: "0d", want: 0},
		{in: "0", want: 0},
		{in: "720h", want: 30 * day},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "-1d", wantErr: true},
		{in: "-5h", wantErr: true},
		{in: "d", wantErr: true},
		{in: "forever", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseRetention(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseRetention(%q) = %v, %v", tt.in, got, err)
		}
	}
}

func TestHealthURL(t *testing.T) {
	tests := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{addr: ":8080", want: "http://127.0.0.1:8080/healthz"},
		{addr: "0.0.0.0:9000", want: "http://127.0.0.1:9000/healthz"},
		{addr: "[::]:9000", want: "http://127.0.0.1:9000/healthz"},
		{addr: "192.168.1.5:8080", want: "http://192.168.1.5:8080/healthz"},
		{addr: "[::1]:8080", want: "http://[::1]:8080/healthz"},
		{addr: "8080", wantErr: true},
	}
	for _, tt := range tests {
		got, err := healthURL(tt.addr)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", tt.addr, got, err, tt.want)
		}
	}
}
