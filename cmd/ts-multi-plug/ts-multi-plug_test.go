package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseAdvertiseTags(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "empty", in: "", want: nil},
		{name: "commas and spaces only", in: " , ", want: nil},
		{name: "single valid tag", in: "tag:dev", want: []string{"tag:dev"}},
		{name: "multiple valid tags", in: "tag:dev,tag:proxy", want: []string{"tag:dev", "tag:proxy"}},
		{name: "whitespace trimming", in: "  tag:dev , tag:proxy  ", want: []string{"tag:dev", "tag:proxy"}},
		{name: "extra commas ignored", in: ",tag:dev,,tag:proxy,", want: []string{"tag:dev", "tag:proxy"}},
		{name: "missing tag prefix", in: "dev", wantErr: true},
		{name: "second entry missing prefix", in: "tag:ok,bad", wantErr: true},
		{name: "empty tag name", in: "tag:", wantErr: true},
		{name: "second entry empty tag name", in: "tag:ok,tag:", wantErr: true},
		{name: "space inside tag", in: "tag:foo bar", wantErr: true},
		{name: "tab inside tag", in: "tag:foo\tbar", wantErr: true},
		{name: "newline inside tag", in: "tag:foo\nbar", wantErr: true},
		{name: "space right after prefix", in: "tag: dev", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAdvertiseTags(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAdvertiseTags(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAdvertiseTags(%q) unexpected error: %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseAdvertiseTags(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewFlagsRegistered(t *testing.T) {
	if f := flag.Lookup("disable-interface-binding"); f == nil {
		t.Fatal("flag -disable-interface-binding not registered")
	} else if f.DefValue != "false" {
		t.Fatalf("-disable-interface-binding default = %q, want %q", f.DefValue, "false")
	}

	if f := flag.Lookup("advertise-tags"); f == nil {
		t.Fatal("flag -advertise-tags not registered")
	} else if f.DefValue != "" {
		t.Fatalf("-advertise-tags default = %q, want %q", f.DefValue, "")
	}
}
