package main

import "testing"

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int // >0 if a>b, 0 if equal, <0 if a<b
	}{
		{"0.8.0", "0.7.1", 1},
		{"0.7.1", "0.8.0", -1},
		{"0.8.0", "0.8.0", 0},
		{"1.0.0", "0.99.99", 1},
		{"0.8.1", "0.8.0", 1},
		{"0.8.0", "0.8.1", -1},
		// Pre-release sorts lower than release.
		{"0.8.0-rc1", "0.8.0", -1},
		{"0.8.0", "0.8.0-rc1", 1},
		// v prefix stripped.
		{"v0.8.0", "0.7.9", 1},
		{"0.8.0", "v0.8.0", 0},
		// Two-part version.
		{"0.8", "0.7.9", 1},
		{"1.0", "0.99", 1},
	}
	for _, tc := range tests {
		got := compareSemver(tc.a, tc.b)
		if (tc.want > 0 && got <= 0) || (tc.want < 0 && got >= 0) || (tc.want == 0 && got != 0) {
			t.Errorf("compareSemver(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPlatformAssetName(t *testing.T) {
	name := platformAssetName()
	if name == "" {
		t.Fatal("platformAssetName returned empty string")
	}
	// Should end with .tar.gz or .zip.
	if len(name) <= 7 || (name[len(name)-7:] != ".tar.gz" && name[len(name)-4:] != ".zip") {
		t.Errorf("platformAssetName = %q, expected .tar.gz or .zip suffix", name)
	}
}

func TestUpdateStateRoundtrip(t *testing.T) {
	state := &UpdateState{
		CurrentVersion:  "0.7.1",
		LatestVersion:   "0.8.0",
		UpdateAvailable: true,
		ChangelogURL:    "https://github.com/SynapsesOS/synapses/releases/tag/v0.8.0",
	}
	// Save.
	saveUpdateState(state)

	// Load.
	loaded := getUpdateState()
	if loaded == nil {
		t.Fatal("getUpdateState returned nil after saveState")
	}
	if loaded.LatestVersion != "0.8.0" {
		t.Errorf("LatestVersion = %q, want %q", loaded.LatestVersion, "0.8.0")
	}
	if !loaded.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true")
	}
}
