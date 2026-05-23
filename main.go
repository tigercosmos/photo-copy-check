package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type problem struct {
	relPath string
	status  string // "missing", "size_mismatch", "hash_mismatch", "read_error"
	detail  string
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".bmp": true, ".tif": true, ".tiff": true, ".webp": true,
	".heic": true, ".heif": true, ".raw": true, ".cr2": true,
	".nef": true, ".arw": true, ".dng": true, ".orf": true,
	".rw2": true, ".raf": true, ".srw": true, ".pef": true,
}

func isImage(name string) bool {
	return imageExts[strings.ToLower(filepath.Ext(name))]
}

func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// resolveDir resolves symlinks on the given path and confirms it points at a
// directory. filepath.Walk uses Lstat on the root, so passing a symlinked
// directory would otherwise be skipped silently.
func resolveDir(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

func collectFiles(root string, imagesOnly bool) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if imagesOnly && !isImage(info.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

// runParallel calls fn on each item using `workers` goroutines, prints a
// progress counter to stderr, and returns any non-nil problems plus the items
// for which fn returned nil (i.e. survived this phase).
func runParallel(label string, items []string, workers int, fn func(string) *problem) (problems []problem, passed []string) {
	if len(items) == 0 {
		return nil, nil
	}
	total := int64(len(items))
	jobs := make(chan string, workers*2)
	var mu sync.Mutex
	var done int64

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				p := fn(rel)
				mu.Lock()
				if p != nil {
					problems = append(problems, *p)
				} else {
					passed = append(passed, rel)
				}
				mu.Unlock()
				d := atomic.AddInt64(&done, 1)
				if d == total || d%100 == 0 {
					fmt.Fprintf(os.Stderr, "\r%s %d / %d", label, d, total)
				}
			}
		}()
	}
	for _, it := range items {
		jobs <- it
	}
	close(jobs)
	wg.Wait()
	fmt.Fprintln(os.Stderr)
	return problems, passed
}

func main() {
	src := flag.String("src", "", "source directory")
	dst := flag.String("dst", "", "copy (destination) directory to verify against the source")
	workers := flag.Int("workers", runtime.NumCPU(), "number of parallel workers")
	imagesOnly := flag.Bool("images-only", true, "only check known image extensions")
	quickSize := flag.Bool("quick", false, "compare file size only (skip hashing — fast but weaker)")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: photo-copy-check -src <source-dir> -dst <copy-dir> [-workers N] [-images-only=false] [-quick]")
		os.Exit(2)
	}
	if *workers < 1 {
		fmt.Fprintf(os.Stderr, "workers must be >= 1 (got %d)\n", *workers)
		os.Exit(2)
	}

	srcResolved, err := resolveDir(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "source is not a readable directory: %v\n", err)
		os.Exit(2)
	}
	dstResolved, err := resolveDir(*dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "destination is not a readable directory: %v\n", err)
		os.Exit(2)
	}

	// Phase 1: list comparison — walk both trees, diff the relative paths.
	fmt.Println("Phase 1/3: scanning file lists...")
	srcFiles, err := collectFiles(srcResolved, *imagesOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to scan source: %v\n", err)
		os.Exit(1)
	}
	dstFiles, err := collectFiles(dstResolved, *imagesOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to scan destination: %v\n", err)
		os.Exit(1)
	}
	dstSet := make(map[string]struct{}, len(dstFiles))
	for _, f := range dstFiles {
		dstSet[f] = struct{}{}
	}
	var problems []problem
	var bothPresent []string
	for _, rel := range srcFiles {
		if _, ok := dstSet[rel]; ok {
			bothPresent = append(bothPresent, rel)
		} else {
			problems = append(problems, problem{rel, "missing", "not present in copy"})
		}
	}
	total := len(srcFiles)
	fmt.Printf("  source: %d, destination: %d, present in both: %d, missing: %d\n",
		len(srcFiles), len(dstFiles), len(bothPresent), total-len(bothPresent))

	// Phase 2: byte-length comparison — stat both sides, compare sizes.
	fmt.Printf("Phase 2/3: comparing byte lengths (%d file(s))...\n", len(bothPresent))
	sizeProblems, sizeMatched := runParallel("  sized", bothPresent, *workers, func(rel string) *problem {
		si, err := os.Stat(filepath.Join(srcResolved, rel))
		if err != nil {
			return &problem{rel, "read_error", fmt.Sprintf("source: %v", err)}
		}
		di, err := os.Stat(filepath.Join(dstResolved, rel))
		if err != nil {
			return &problem{rel, "read_error", fmt.Sprintf("copy: %v", err)}
		}
		if si.Size() != di.Size() {
			return &problem{rel, "size_mismatch",
				fmt.Sprintf("src=%d bytes, copy=%d bytes", si.Size(), di.Size())}
		}
		return nil
	})
	problems = append(problems, sizeProblems...)

	// Phase 3: SHA-256 — only files that survived phase 2.
	if *quickSize {
		fmt.Println("Phase 3/3: skipped (-quick).")
	} else {
		fmt.Printf("Phase 3/3: comparing SHA-256 (%d file(s))...\n", len(sizeMatched))
		hashProblems, _ := runParallel("  hashed", sizeMatched, *workers, func(rel string) *problem {
			sh, err := hashFile(filepath.Join(srcResolved, rel))
			if err != nil {
				return &problem{rel, "read_error", fmt.Sprintf("hash source: %v", err)}
			}
			dh, err := hashFile(filepath.Join(dstResolved, rel))
			if err != nil {
				return &problem{rel, "read_error", fmt.Sprintf("hash copy: %v", err)}
			}
			if string(sh) != string(dh) {
				return &problem{rel, "hash_mismatch", "content differs"}
			}
			return nil
		})
		problems = append(problems, hashProblems...)
	}

	if len(problems) == 0 {
		fmt.Printf("OK: all %d file(s) match.\n", total)
		return
	}

	sort.Slice(problems, func(i, j int) bool {
		if problems[i].status != problems[j].status {
			return problems[i].status < problems[j].status
		}
		return problems[i].relPath < problems[j].relPath
	})

	counts := map[string]int{}
	for _, p := range problems {
		counts[p.status]++
	}

	fmt.Printf("FAIL: %d file(s) had issues out of %d.\n", len(problems), total)
	for _, k := range []string{"missing", "size_mismatch", "hash_mismatch", "read_error"} {
		if counts[k] > 0 {
			fmt.Printf("  %s: %d\n", k, counts[k])
		}
	}
	fmt.Println()
	for _, p := range problems {
		fmt.Printf("[%s] %s  (%s)\n", p.status, p.relPath, p.detail)
	}
	os.Exit(1)
}
