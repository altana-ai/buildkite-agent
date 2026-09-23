package jobcgroup

import "testing"

func TestParseMode(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]Mode{
		"":        ModeOff,
		"off":     ModeOff,
		"report":  ModeReport,
		"enforce": ModeEnforce,
	} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v, want %q, nil", in, got, err, want)
		}
	}

	for _, in := range []string{"on", "Enforce", "true"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) error = nil, want an error", in)
		}
	}
}
