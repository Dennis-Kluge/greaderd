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
			want: config{addr: ":8080", dbPath: "greaderd.db", refreshInterval: 15 * time.Minute},
		},
		{
			name: "overrides",
			env: map[string]string{
				"GREADERD_USER": "u", "GREADERD_PASSWORD": "p",
				"GREADERD_ADDR": "127.0.0.1:9000", "GREADERD_DB": "/tmp/x.db", "GREADERD_REFRESH_INTERVAL": "5m",
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
