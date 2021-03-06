// Copyright 2015-2017 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/u-root/u-root/pkg/cpio"
	"github.com/u-root/u-root/pkg/ramfs"
)

var (
	config struct {
		Goroot          string
		Arch            string
		Goos            string
		Gopath          string
		TempDir         string
		Go              string
		InitialCpio     string
		UseExistingInit bool
	}

	// be VERY CAREFUL with these. If you have an empty line here it will
	// result in cpio copying the whole tree.
	goList         = []string{"pkg/include"}
	urootList      []string
	pkgList        []string
	deps           map[string]bool
	gorootFiles    map[string]bool
	urootFiles     map[string]bool
	standardgotool = true
)

func init() {
	flag.BoolVar(&config.UseExistingInit, "useinit", false, "If there is an existing init, don't replace it")
	flag.StringVar(&config.InitialCpio, "cpio", "", "An initial cpio image to build on")
	flag.StringVar(&config.TempDir, "tmpdir", "", "tmpdir to use instead of ioutil.TempDir")
}

func buildPkg(pkg string, wd string, output string, opts []string) error {
	args := []string{
		"build", "-x", "-a",
		"-o", output,
		"-installsuffix", "cgo",
		"-ldflags", "-s -w",
	}
	if opts != nil {
		args = append(args, opts...)
	}
	if pkg != "" {
		args = append(args, pkg)
	}

	cmd := exec.Command("go", args...)
	if wd != "" {
		cmd.Dir = wd
	}
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if o, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building statically linked go tool info %v: %v, %v", pkg, string(o), err)
	}
	return nil
}

// buildToolChain builds the four binaries needed for the go toolchain:
// go, compile, link, and asm. We do this to ensure we get smaller binaries.
// Smaller, in this, meaning 25M instead of 33M. What a world!
func buildToolChain() error {
	log.Printf("Building go tools...")

	goBin := filepath.Join(config.TempDir, "go/bin/go")
	goDir := filepath.Join(config.Goroot, "src/cmd/go")
	if err := buildPkg("", goDir, goBin, []string{"-tags", "cmd_go_bootstrap"}); err != nil {
		return err
	}

	toolDir := filepath.Join(config.TempDir, fmt.Sprintf("go/pkg/tool/%v_%v", config.Goos, config.Arch))
	for _, pkg := range []string{"compile", "link", "asm"} {
		c := filepath.Join(toolDir, pkg)
		if err := buildPkg(fmt.Sprintf("cmd/%s", pkg), "", c, nil); err != nil {
			return err
		}
	}
	return nil
}

func guessgoarch() {
	if arch := os.Getenv("GOARCH"); arch != "" {
		config.Arch = filepath.Clean(arch)
	} else {
		config.Arch = runtime.GOARCH
	}
}

func guessgoroot() {
	if root := os.Getenv("GOROOT"); root != "" {
		config.Goroot = filepath.Clean(root)
	} else {
		config.Goroot = runtime.GOROOT()
	}
	log.Printf("Using %q as GOROOT", config.Goroot)
}

func guessgopath() {
	gopath := os.Getenv("GOPATH")
	if gopath != "" {
		config.Gopath = gopath
		return
	}
	log.Fatalf("You have to set GOPATH, which is typically ~/go")
}

type goPackage struct {
	Dir        string
	Deps       []string
	GoFiles    []string
	SFiles     []string
	HFiles     []string
	Goroot     bool
	ImportPath string
}

// goListPkg takes one package name, and computes all the files it needs to
// build, separating them into Go tree files and uroot files. For now we just
// 'go list' but hopefully later we can do this programmatically.
func goListPkg(name string) (*goPackage, error) {
	cmd := exec.Command("go", "list", "-json", name)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	j, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	var p goPackage
	if err := json.Unmarshal([]byte(j), &p); err != nil {
		return nil, err
	}

	for _, v := range append(append(p.GoFiles, p.SFiles...), p.HFiles...) {
		if p.Goroot {
			gorootFiles[filepath.Join(p.ImportPath, v)] = true
		} else {
			urootFiles[filepath.Join(p.ImportPath, v)] = true
		}
	}
	return &p, nil
}

// addGoFiles Computes the set of Go files to be added to the initramfs.
func addGoFiles() error {
	// For each directory in pkgList, add its files and all its
	// dependencies.  It would be nice to run go list -json with
	// lots of package names but it produces invalid JSON.  It
	// produces a stream thatis {}{}{} at the top level and the
	// decoders don't like that.
	for _, v := range pkgList {
		p, err := goListPkg(v)
		if err != nil {
			log.Printf("Can't do go list in %v, ignoring\n", v)
			continue
		}
		for _, v := range p.Deps {
			deps[v] = true
		}
	}

	for v := range deps {
		if _, err := goListPkg(v); err != nil {
			log.Fatalf("%v", err)
		}
	}
	for v := range gorootFiles {
		goList = append(goList, filepath.Join("src", v))
	}
	for v := range urootFiles {
		urootList = append(urootList, filepath.Join("src", v))
	}
	return nil
}

func globlist(s ...string) []string {
	// For each arg, use it as a Glob pattern and add any matches to the
	// package list. If there are no arguments, use [a-zA-Z]* as the glob pattern.
	var pat []string
	for _, v := range s {
		pat = append(pat, filepath.Join(config.Gopath, v))
	}
	if len(s) == 0 {
		pat = []string{filepath.Join(config.Gopath, "src/github.com/u-root/u-root/cmds", "[a-zA-Z]*")}
	}
	return pat
}

func main() {
	flag.Parse()

	deps = make(map[string]bool)
	gorootFiles = make(map[string]bool)
	urootFiles = make(map[string]bool)

	guessgoarch()
	config.Go = ""
	config.Goos = "linux"
	guessgoroot()
	guessgopath()

	pat := globlist(flag.Args()...)

	for _, v := range pat {
		g, err := filepath.Glob(v)
		if err != nil {
			log.Fatalf("Glob error: %v", err)
		}
		// We have a set of absolute paths in g.  We can not
		// use absolute paths in go list, however, so we have
		// to adjust them.
		for i := range g {
			r, err := filepath.Rel(filepath.Join(config.Gopath, "src"), g[i])
			if err != nil {
				log.Fatalf("Can't get rel path for %v: %v", g, err)
			}
			g[i] = r
		}
		pkgList = append(pkgList, g...)
	}

	if err := addGoFiles(); err != nil {
		log.Fatalf("%v", err)
	}

	if config.TempDir == "" {
		var err error
		config.TempDir, err = ioutil.TempDir("", "u-root")
		if err != nil {
			log.Fatalf("%v", err)
		}
	}

	defer func() {
		log.Printf("Removing %v", config.TempDir)
		if err := os.RemoveAll(config.TempDir); err != nil {
			log.Fatalf("%v", err)
		}
	}()

	if err := buildToolChain(); err != nil {
		log.Fatalf("%v", err)
	}

	if !config.UseExistingInit {
		init := filepath.Join(config.TempDir, "init")
		dir := filepath.Join(config.Gopath, "src/github.com/u-root/u-root/cmds/init")

		if err := buildPkg(".", dir, init, nil); err != nil {
			log.Fatalf("%v", err)
		}
	}

	oname := fmt.Sprintf("/tmp/initramfs.%v_%v.cpio", config.Goos, config.Arch)
	f, err := os.Create(oname)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer f.Close()

	archiver, err := cpio.Format("newc")
	if err != nil {
		log.Fatalf("%v", err)
	}

	init, err := ramfs.NewInitramfs(archiver.Writer(f))
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Start with the initial CPIO.
	if config.InitialCpio != "" {
		initial, err := os.Open(config.InitialCpio)
		if err != nil {
			log.Fatalf("%v", err)
		}

		transform := cpio.MakeReproducible
		if !config.UseExistingInit {
			transform = func(r cpio.Record) cpio.Record {
				// Rename init to inito.
				if r.Name == "init" {
					r.Name = "inito"
				}
				return cpio.MakeReproducible(r)
			}
		}

		if err := init.Concat(archiver.Reader(initial), transform); err != nil {
			log.Fatalf("%v", err)
		}
	}

	// Write all Go toolchain files to the archive.
	if err := init.WriteFiles(config.Goroot, "go", goList); err != nil {
		log.Fatalf("%v", err)
	}

	// Write u-root src files to the archive.
	if err := init.WriteFiles(config.Gopath, "", urootList); err != nil {
		log.Fatalf("%v", err)
	}

	// Write all files from the TempDir.
	if err := init.WriteFile(config.TempDir, ""); err != nil {
		log.Fatalf("%v", err)
	}

	if err := init.WriteTrailer(); err != nil {
		log.Fatalf("%v", err)
	}

	log.Printf("Output file is %s", oname)
}
