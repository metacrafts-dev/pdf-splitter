package cli

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestParsePoints(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []int
		wantErr bool
	}{
		{
			name:  "comma separated",
			input: "10,20,40",
			want:  []int{10, 20, 40},
		},
		{
			name:  "space separated",
			input: "10 20 40",
			want:  []int{10, 20, 40},
		},
		{
			name:  "mixed comma and space",
			input: " 10, 20 , 40 ",
			want:  []int{10, 20, 40},
		},
		{
			name:    "invalid numbers",
			input:   "10,abc,40",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePoints(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParsePoints() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParsePoints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCLIOptionsAndEnvDefaults(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("RETENTION", "45m")
	t.Setenv("MAX_UPLOAD_MB", "250")
	t.Setenv("HEADLESS", "true")

	var errBuf bytes.Buffer
	opts, err := parseCLIOptions([]string{"-server"}, &errBuf)
	if err != nil {
		t.Fatalf("parseCLIOptions failed: %v", err)
	}
	if opts.portFlag != 9999 {
		t.Errorf("expected port 9999 from env, got %d", opts.portFlag)
	}
	if opts.hostFlag != "0.0.0.0" || !opts.hostExplicit {
		t.Errorf("expected host 0.0.0.0 (explicit=true) from env, got %q (explicit=%v)", opts.hostFlag, opts.hostExplicit)
	}
	if opts.retentionFlag != "45m" {
		t.Errorf("expected retention 45m, got %q", opts.retentionFlag)
	}
	if opts.maxUploadMBFlag != 250 {
		t.Errorf("expected maxUploadMB 250, got %d", opts.maxUploadMBFlag)
	}
	if !opts.headlessFlag {
		t.Errorf("expected headless true from env")
	}
}

func TestParseCLIOptionsBooleanFlagWithExplicitValue(t *testing.T) {
	t.Setenv("HEADLESS", "true")
	var errBuf bytes.Buffer
	opts, err := parseCLIOptions([]string{"-server", "-headless", "false"}, &errBuf)
	if err != nil {
		t.Fatalf("parseCLIOptions failed: %v", err)
	}
	if opts.headlessFlag {
		t.Errorf("expected -headless false to disable headless")
	}
	if len(opts.positional) != 0 {
		t.Errorf("expected no positional args, got %v", opts.positional)
	}
}

func TestRunHelp(t *testing.T) {
	if err := Run(context.Background(), []string{"-help"}); err != nil {
		t.Fatalf("Run(-help) failed: %v", err)
	}
}
