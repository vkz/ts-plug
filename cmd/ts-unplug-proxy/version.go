package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
var version = "dev"

func printVersion() {
	fmt.Printf("ts-unplug-proxy %s\n", version)
	fmt.Printf("go: %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		fmt.Printf("module: %s %s\n", bi.Main.Path, bi.Main.Version)
	} else if bi.Main.Path != "" {
		fmt.Printf("module: %s\n", bi.Main.Path)
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision", "vcs.time", "vcs.modified":
			fmt.Printf("%s: %s\n", s.Key, s.Value)
		}
	}
}
