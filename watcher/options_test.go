package watcher

import (
	"testing"
	"time"
)

func TestWithDefaultPollInterval(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"positive applied", 5 * time.Second, 5 * time.Second},
		{"zero ignored keeps default", 0, DefaultPollInterval},
		{"negative ignored keeps default", -1 * time.Second, DefaultPollInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewExternalWatcher(&testFetcher{}, WithDefaultPollInterval(tt.in))
			if w.defaultPollInterval != tt.want {
				t.Errorf("defaultPollInterval = %v, want %v", w.defaultPollInterval, tt.want)
			}
		})
	}
}

func TestWithPollJitter(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want float64
	}{
		{"default", nil, DefaultPollJitter},
		{"explicit", []Option{WithPollJitter(0.5)}, 0.5},
		{"disabled", []Option{WithPollJitter(0)}, 0},
		{"negative clamped to zero", []Option{WithPollJitter(-1)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewExternalWatcher(&testFetcher{}, tt.opts...)
			if w.pollJitter != tt.want {
				t.Errorf("pollJitter = %v, want %v", w.pollJitter, tt.want)
			}
		})
	}
}

func TestReadinessRetryConfig_JitterDefaults(t *testing.T) {
	ptrTo := func(v float64) *float64 { return &v }
	tests := []struct {
		name string
		in   *float64
		want float64
	}{
		{"nil defaults to 0.1", nil, DefaultReadinessRetryJitter},
		{"explicit zero disables", ptrTo(0), 0},
		{"explicit value kept", ptrTo(0.3), 0.3},
		{"negative treated as unset", ptrTo(-1), DefaultReadinessRetryJitter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReadinessRetryConfig{Jitter: tt.in}.withDefaults().Jitter
			if got == nil {
				t.Fatal("Jitter is nil after withDefaults")
			}
			if *got != tt.want {
				t.Errorf("Jitter = %v, want %v", *got, tt.want)
			}
		})
	}
}
