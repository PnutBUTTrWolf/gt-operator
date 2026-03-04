package shim

import "testing"

func TestExtractSessionTarget(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "send-keys with -t space",
			args: []string{"send-keys", "-t", "gt-toast", "echo hello", "C-m"},
			want: "gt-toast",
		},
		{
			name: "send-keys with -t no space",
			args: []string{"send-keys", "-tgt-toast", "echo hello", "C-m"},
			want: "gt-toast",
		},
		{
			name: "has-session with -t",
			args: []string{"has-session", "-t", "hq-mayor"},
			want: "hq-mayor",
		},
		{
			name: "strip pane suffix",
			args: []string{"send-keys", "-t", "gt-toast:0.0", "ls", "C-m"},
			want: "gt-toast",
		},
		{
			name: "strip window suffix",
			args: []string{"send-keys", "-t", "gt-toast:main", "ls", "C-m"},
			want: "gt-toast",
		},
		{
			name: "no target flag",
			args: []string{"ls"},
			want: "",
		},
		{
			name: "new-session no target",
			args: []string{"new-session", "-d", "-s", "gt-test"},
			want: "",
		},
		{
			name: "-t at end without value",
			args: []string{"has-session", "-t"},
			want: "",
		},
		{
			name: "empty args",
			args: []string{},
			want: "",
		},
		{
			name: "pane suffix with -t no space",
			args: []string{"send-keys", "-thq-mayor:0.1", "cmd", "C-m"},
			want: "hq-mayor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSessionTarget(tt.args)
			if got != tt.want {
				t.Errorf("extractSessionTarget(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestMapRouter(t *testing.T) {
	r := NewMapRouter()

	// Initially empty
	if pod := r.PodForSession("gt-toast"); pod != "" {
		t.Errorf("PodForSession on empty router = %q, want empty", pod)
	}

	// Register and lookup
	r.Register("gt-toast", "polecat-toast")
	if pod := r.PodForSession("gt-toast"); pod != "polecat-toast" {
		t.Errorf("PodForSession after register = %q, want %q", pod, "polecat-toast")
	}

	// Different session still empty
	if pod := r.PodForSession("gt-other"); pod != "" {
		t.Errorf("PodForSession for unknown session = %q, want empty", pod)
	}

	// Unregister
	r.Unregister("gt-toast")
	if pod := r.PodForSession("gt-toast"); pod != "" {
		t.Errorf("PodForSession after unregister = %q, want empty", pod)
	}
}

func TestNewShim(t *testing.T) {
	s := NewShim(ModeAgent, "gastown")
	if s.Mode != ModeAgent {
		t.Errorf("Mode = %v, want ModeAgent", s.Mode)
	}
	if s.Namespace != "gastown" {
		t.Errorf("Namespace = %q, want %q", s.Namespace, "gastown")
	}
	// Default real tmux path
	if s.RealTmux != "/usr/bin/tmux" {
		t.Errorf("RealTmux = %q, want %q", s.RealTmux, "/usr/bin/tmux")
	}
}

func TestNewShimWithEnv(t *testing.T) {
	t.Setenv("GT_REAL_TMUX", "/opt/tmux/bin/tmux")
	s := NewShim(ModeOperator, "test-ns")
	if s.RealTmux != "/opt/tmux/bin/tmux" {
		t.Errorf("RealTmux = %q, want %q", s.RealTmux, "/opt/tmux/bin/tmux")
	}
}
