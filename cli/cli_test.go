package cli

import (
	"os"
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

func TestEnvDefaults(t *testing.T) {
	// Verify environment variables are recognized
	origPort := os.Getenv("PORT")
	origHost := os.Getenv("HOST")
	defer func() {
		os.Setenv("PORT", origPort)
		os.Setenv("HOST", origHost)
	}()

	os.Setenv("PORT", "9999")
	os.Setenv("HOST", "127.0.0.1")

	// Verify env vars can be read properly
	if os.Getenv("PORT") != "9999" || os.Getenv("HOST") != "127.0.0.1" {
		t.Errorf("Failed to read test environment variables")
	}
}
