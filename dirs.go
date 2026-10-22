package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type DirEntry struct {
	Name string
	Path string
	Git  bool // contains a .git entry
}

type Crumb struct {
	Name string
	Path string
}

type Project struct {
	Name      string
	Path      string
	Git       bool
	Sessions  int
	LastUsed  time.Time
}

// listDir returns the subdirectories inside dir and whether dir itself is a git repo.
func listDir(dir string, hidden bool) ([]DirEntry, bool, string) {
	_, gitErr := os.Lstat(filepath.Join(dir, ".git"))
	isGit := gitErr == nil

	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, isGit, err.Error()
	}
	var out []DirEntry
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !hidden {
			continue
		}
		p := filepath.Join(dir, name)
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			st, err := os.Stat(p)
			isDir = err == nil && st.IsDir()
		}
		if !isDir {
			continue
		}
		_, gErr := os.Lstat(filepath.Join(p, ".git"))
		out = append(out, DirEntry{Name: name, Path: p, Git: gErr == nil})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Git != out[j].Git {
			return out[i].Git
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, isGit, ""
}

// breadcrumbs returns the clickable path segments from root (or "/") down to dir.
func breadcrumbs(root, dir string) []Crumb {
	dir = filepath.Clean(dir)
	root = filepath.Clean(root)
	if rel, err := filepath.Rel(root, dir); err != nil || strings.HasPrefix(rel, "..") {
		root = "/"
	}

	var chain []Crumb
	for p := dir; ; p = filepath.Dir(p) {
		name := filepath.Base(p)
		if p == root && root != "/" {
			name = "~"
		}
		chain = append([]Crumb{{Name: name, Path: p}}, chain...)
		if p == root || p == "/" {
			break
		}
	}
	return chain
}

var skipScanDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"dist":         true,
	"build":        true,
	"Library":      true,
	"Applications": true,
	"Pictures":     true,
	"Music":        true,
	"Movies":       true,
	"Videos":       true,
	"Downloads":    true,
	"Public":       true,
	"go":           true,
	"snap":         true,
}

// discoverProjects merges recent session directories with git repositories
// found within 3 levels under root. Recent directories come first (ordered by
// most recent session), followed by discovered git repos alphabetically.
func discoverProjects(root string, sessions []SessionInfo) []Project {
	byPath := map[string]*Project{}
	var recent []*Project

	for _, s := range sessions {
		if s.Cwd == "" {
			continue
		}
		p := filepath.Clean(s.Cwd)
		proj, ok := byPath[p]
		if !ok {
			st, err := os.Stat(p)
			if err != nil || !st.IsDir() {
				continue
			}
			_, gitErr := os.Lstat(filepath.Join(p, ".git"))
			proj = &Project{
				Name: filepath.Base(p),
				Path: p,
				Git:  gitErr == nil,
			}
			byPath[p] = proj
			recent = append(recent, proj)
		}
		proj.Sessions++
		if s.Modified.After(proj.LastUsed) {
			proj.LastUsed = s.Modified
		}
	}

	sort.Slice(recent, func(i, j int) bool {
		return recent[i].LastUsed.After(recent[j].LastUsed)
	})

	var discovered []Project
	var scan func(dir string, depth int)
	scan = func(dir string, depth int) {
		if depth > 3 {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			name := e.Name()
			if strings.HasPrefix(name, ".") || skipScanDirs[name] {
				continue
			}
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(dir, name)
			_, gitErr := os.Lstat(filepath.Join(p, ".git"))
			if gitErr == nil {
				if _, seen := byPath[p]; !seen {
					byPath[p] = &Project{Name: name, Path: p, Git: true}
					discovered = append(discovered, *byPath[p])
				}
				continue // don't descend inside git repos
			}
			scan(p, depth+1)
		}
	}
	if root != "" {
		scan(filepath.Clean(root), 1)
	}

	sort.Slice(discovered, func(i, j int) bool {
		return strings.ToLower(discovered[i].Name) < strings.ToLower(discovered[j].Name)
	})

	out := make([]Project, 0, len(recent)+len(discovered))
	for _, r := range recent {
		out = append(out, *r)
	}
	out = append(out, discovered...)
	return out
}

