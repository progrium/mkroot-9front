package iso9660

import "testing"

func TestJolietName(t *testing.T) {
	for raw, want := range map[string]string{
		"\x00r\x00c\x00;\x001":                          "rc",
		"\x00n\x00a\x00m\x00e\x00s\x00p\x00a\x00c\x00e": "namespace",
		"\x00\xe9\x00t\x00\xe9":                         "été",
	} {
		if got := jolietName([]byte(raw)); got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
}

func TestPlan9(t *testing.T) {
	// A file record named "RC" (even, so one pad byte) with no Plan 9
	// name, uid and gid "sys", and mode 0775.
	raw := make([]byte, 32)
	raw = append(raw, 2, 'R', 'C', 0, 0, 3, 's', 'y', 's', 3, 's', 'y', 's', 0, 0xfd, 0x01, 0, 0)
	m := record(raw).plan9()
	if !m.ok || m.mode != 0o775 || m.name != "" || m.uid != "sys" || m.gid != "sys" {
		t.Fatalf("%+v", m)
	}
}
