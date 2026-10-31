package main

import "testing"

func TestFitWindowSize(t *testing.T) {
	cases := []struct {
		name               string
		workW, workH       int
		wantWidth, wantHgt int
	}{
		{"large screen", 2560, 1400, windowWidth, windowHeight},
		{"1080p at 125%", 1536, 824, windowWidth, 824},
		{"1080p at 150%", 1280, 680, 1280, 680},
		{"tiny stays at the minimum", 800, 500, windowMinWidth, windowMinHeight},
		{"unknown work area", 0, 0, windowWidth, windowHeight},
	}
	for _, c := range cases {
		w, h := fitWindowSize(c.workW, c.workH, windowWidth, windowHeight, windowMinWidth, windowMinHeight)
		if w != c.wantWidth || h != c.wantHgt {
			t.Errorf("%s: got %dx%d, want %dx%d", c.name, w, h, c.wantWidth, c.wantHgt)
		}
	}
}
