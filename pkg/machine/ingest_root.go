package machine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ingest_root.go — the statements root, path containment and the staging directory
// (apis.mdx §14.2, §14.3, §14.9).
//
// The statements tree is the operator's audit evidence: strictly read-only. The ONLY directory
// under it this plane ever writes is the staging directory, {ROOT}/.ezbk-staging/ by default.
// Every path a caller names is resolved to its real path (symlinks followed) and must stay inside
// the real root; a path that escapes it is refused as not_found (it is not ours to reveal).

const (
	ingDefaultStaging  = ".ezbk-staging"
	ingStagingMarker   = ".ezbk-staging.json"
	ingEnvStatementDir = "EZBK_STATEMENTS_DIR"
)

// ingRoot is a resolved statements root
type ingRoot struct {
	// Display is the root as configured or given (echoed to the caller; it is the operator's own
	// setting, so naming it leaks nothing they did not tell us)
	Display string
	// Real is the absolute real path (symlinks resolved) that containment is checked against
	Real   string
	Source string
}

// ingConfiguredRoot returns the configured statements root and where it came from
func ingConfiguredRoot() (string, string) {
	if creds, err := ReadCredentials(); err == nil && strings.TrimSpace(creds.StatementsRoot) != "" {
		return strings.TrimSpace(creds.StatementsRoot), "ezbookkeeping.statements.root"
	}

	if v := strings.TrimSpace(os.Getenv(ingEnvStatementDir)); v != "" {
		return v, ingEnvStatementDir
	}

	return "", ""
}

// ingExpandHome expands a leading ~/
func ingExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}

	return p
}

// ingResolveRoot resolves the root argument, or the configured root when the argument is empty
func ingResolveRoot(arg string) (*ingRoot, error) {
	display := strings.TrimSpace(arg)
	source := "argument"

	if display == "" {
		display, source = ingConfiguredRoot()
	}

	if display == "" {
		return nil, Invalid("pass root, or set ezbookkeeping.statements.root in ~/.credentials/ezbookkeeping.json (the CLI's --path / EZBK_STATEMENTS_DIR pass it as root)", "no statements root is configured")
	}

	expanded := ingExpandHome(display)

	if !filepath.IsAbs(expanded) {
		return nil, Invalid("pass root as an absolute path", "the statements root %q is not an absolute path", display)
	}

	real, err := filepath.EvalSymlinks(filepath.Clean(expanded))

	if err != nil {
		return nil, NotFound("check the statements root exists and is readable by the server's user", "the statements root %q cannot be resolved", display)
	}

	info, err := os.Stat(real)

	if err != nil || !info.IsDir() {
		return nil, NotFound("point root at the directory that holds the statements tree", "the statements root %q is not a directory", display)
	}

	return &ingRoot{Display: display, Real: real, Source: source}, nil
}

// ingWithin reports whether real is root or below it
func ingWithin(root, real string) bool {
	if real == root {
		return true
	}

	sep := string(filepath.Separator)
	prefix := root

	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}

	return strings.HasPrefix(real, prefix)
}

// Resolve turns a path relative to the root (or absolute) into its real path, refusing anything
// that escapes the root. The path need not exist: the nearest existing ancestor is resolved and the
// rest appended, so a staging path can be checked before it is created.
func (r *ingRoot) Resolve(p string) (string, error) {
	p = strings.TrimSpace(p)

	if p == "" {
		return r.Real, nil
	}

	p = ingExpandHome(p)

	var joined string

	if filepath.IsAbs(p) {
		joined = filepath.Clean(p)
	} else {
		joined = filepath.Join(r.Real, p)
	}

	real, err := ingEvalExistingPrefix(joined)

	if err != nil {
		return "", NotFound("paths are relative to the statements root and must stay inside it", "the path %q cannot be resolved", p)
	}

	if !ingWithin(r.Real, real) {
		return "", NotFound("paths are relative to the statements root and must stay inside it", "the path %q is outside the statements root", p)
	}

	return real, nil
}

// Rel renders a real path relative to the root, with forward slashes
func (r *ingRoot) Rel(real string) string {
	rel, err := filepath.Rel(r.Real, real)

	if err != nil {
		return filepath.ToSlash(filepath.Base(real))
	}

	if rel == "." {
		return ""
	}

	return filepath.ToSlash(rel)
}

// ingEvalExistingPrefix resolves symlinks of the longest existing prefix of p
func ingEvalExistingPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	cur := p

	for {
		real, err := filepath.EvalSymlinks(cur)

		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}

			return filepath.Clean(real), nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(cur)

		if parent == cur {
			return "", err
		}

		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// ingStaging is the resolved staging directory of a root
type ingStaging struct {
	Root *ingRoot
	Dir  string // real path
	Rel  string // relative to root
}

type ingStagingMarkerFile struct {
	App     string `json:"app"`
	Purpose string `json:"purpose"`
	Created string `json:"created"`
}

// ingResolveStaging resolves the staging directory for a root without creating it. manifestRel is
// the manifest's path relative to the root (may be empty); the LOCKED conflict rule refuses a
// staging directory that is the manifest's own directory.
func ingResolveStaging(root *ingRoot, arg, manifestRel string) (*ingStaging, error) {
	rel := strings.TrimSpace(arg)

	if rel == "" {
		rel = ingDefaultStaging
	}

	if filepath.IsAbs(ingExpandHome(rel)) {
		return nil, Invalid("pass staging as a path relative to the statements root, e.g. .ezbk-staging", "staging must be relative to the statements root")
	}

	dir, err := root.Resolve(rel)

	if err != nil {
		return nil, err
	}

	if dir == root.Real {
		return nil, Conflict("stage into a dedicated directory such as .ezbk-staging", "the staging directory cannot be the statements root itself")
	}

	if manifestRel != "" {
		if mdir, merr := root.Resolve(filepath.Dir(manifestRel)); merr == nil && mdir != root.Real && (dir == mdir || ingWithin(mdir, dir)) {
			return nil, Conflict("leave staging at the default .ezbk-staging/; the manifest's directory belongs to the archive", "staging %q would write into the manifest's directory %q", root.Rel(dir), root.Rel(mdir)).WithDetails(map[string]any{"staging": root.Rel(dir), "manifest_dir": root.Rel(mdir)})
		}
	}

	return &ingStaging{Root: root, Dir: dir, Rel: root.Rel(dir)}, nil
}

// Exists reports whether the staging directory exists and is ours
func (s *ingStaging) Exists() bool {
	_, err := os.Stat(filepath.Join(s.Dir, ingStagingMarker))
	return err == nil
}

// Ensure creates the staging directory (with its .gitignore of "*" and our marker), refusing a
// directory that already holds files we did not write
func (s *ingStaging) Ensure() error {
	info, err := os.Stat(s.Dir)

	if err == nil {
		if !info.IsDir() {
			return Conflict("remove or rename the file, or pass a different staging path", "the staging path %q exists and is not a directory", s.Rel)
		}

		if !s.Exists() {
			entries, rerr := os.ReadDir(s.Dir)

			if rerr != nil {
				return NotFound("check the server's user can read the staging directory", "the staging directory %q cannot be read", s.Rel)
			}

			var foreign []string

			for _, e := range entries {
				if e.Name() == ".gitignore" {
					continue
				}

				foreign = append(foreign, e.Name())
			}

			if len(foreign) > 0 {
				if len(foreign) > 10 {
					foreign = foreign[:10]
				}

				return Conflict("pass a different staging directory; this one holds files the plane did not write", "the staging directory %q already holds foreign files", s.Rel).WithDetails(map[string]any{"staging": s.Rel, "root": s.Root.Display, "foreign": foreign})
			}
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if merr := os.MkdirAll(s.Dir, 0o700); merr != nil {
			return NewFail(CodeInternal, "check the server's user can write under the statements root", "cannot create the staging directory %q", s.Rel)
		}
	} else {
		return NewFail(CodeInternal, "check the server's user can read the statements root", "cannot inspect the staging directory %q", s.Rel)
	}

	// re-check containment after creation (a symlink could have been planted meanwhile)
	real, err := filepath.EvalSymlinks(s.Dir)

	if err != nil || !ingWithin(s.Root.Real, real) || real == s.Root.Real {
		return NotFound("the staging directory must stay inside the statements root", "the staging directory %q resolves outside the statements root", s.Rel)
	}

	s.Dir = real

	gi := filepath.Join(s.Dir, ".gitignore")

	if _, err := os.Lstat(gi); errors.Is(err, fs.ErrNotExist) {
		if werr := ingWriteFileAtomic(s.Dir, gi, []byte("*\n")); werr != nil {
			return werr
		}
	}

	if !s.Exists() {
		marker, _ := json.MarshalIndent(ingStagingMarkerFile{App: "ezbookkeeping machine plane", Purpose: "derived, rebuildable ingest staging; safe to delete", Created: time.Now().UTC().Format(time.RFC3339)}, "", "  ")

		if werr := ingWriteFileAtomic(s.Dir, filepath.Join(s.Dir, ingStagingMarker), marker); werr != nil {
			return werr
		}
	}

	return nil
}

// Path returns the real path of a file inside staging, refusing anything that escapes it
func (s *ingStaging) Path(rel string) (string, error) {
	p := filepath.Join(s.Dir, filepath.FromSlash(rel))
	real, err := ingEvalExistingPrefix(p)

	if err != nil || !ingWithin(s.Dir, real) || real == s.Dir {
		return "", NotFound("staging paths stay inside the staging directory", "the staging path %q is invalid", rel)
	}

	return real, nil
}

// WriteFile writes a file inside staging atomically (temp + rename), creating parents
func (s *ingStaging) WriteFile(rel string, data []byte) error {
	p, err := s.Path(rel)

	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return NewFail(CodeInternal, "check the server's user can write the staging directory", "cannot create a staging subdirectory")
	}

	return ingWriteFileAtomic(s.Dir, p, data)
}

// ReadFile reads a file inside staging; a missing file returns (nil, nil)
func (s *ingStaging) ReadFile(rel string) ([]byte, error) {
	p, err := s.Path(rel)

	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(p)

	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, NewFail(CodeInternal, "check the server's user can read the staging directory", "cannot read staging file %q", rel)
	}

	return data, nil
}

// AppendLine appends one line to a staging file (the run log is append-only)
func (s *ingStaging) AppendLine(rel, line string) error {
	p, err := s.Path(rel)

	if err != nil {
		return err
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)

	if err != nil {
		return NewFail(CodeInternal, "check the server's user can write the staging directory", "cannot append to %q", rel)
	}

	defer f.Close()

	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}

	_, err = f.WriteString(line)

	return err
}

// Remove deletes a file inside staging (only ever our own derived files)
func (s *ingStaging) Remove(rel string) error {
	p, err := s.Path(rel)

	if err != nil {
		return err
	}

	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return NewFail(CodeInternal, "check the server's user can write the staging directory", "cannot remove staging file %q", rel)
	}

	return nil
}

// ingWriteFileAtomic writes data to path via a temp file in the same directory and a rename. The
// directory must be inside allowedDir (a belt-and-braces check that nothing outside staging or the
// state directory is ever written).
func ingWriteFileAtomic(allowedDir, path string, data []byte) error {
	dir := filepath.Dir(path)

	if !ingWithin(allowedDir, dir) {
		return NewFail(CodeInternal, "this is a bug; report it", "refusing to write outside the allowed directory")
	}

	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(suffix))

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)

	if err != nil {
		return NewFail(CodeInternal, "check the server's user can write the directory", "cannot write %s", filepath.Base(path))
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return NewFail(CodeInternal, "check free disk space", "cannot write %s", filepath.Base(path))
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return NewFail(CodeInternal, "check free disk space", "cannot sync %s", filepath.Base(path))
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return NewFail(CodeInternal, "check free disk space", "cannot close %s", filepath.Base(path))
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return NewFail(CodeInternal, "check the server's user can write the directory", "cannot replace %s", filepath.Base(path))
	}

	return nil
}

// ingSkipDir reports directories the walker never descends into
func ingSkipDir(name string) bool {
	if name == ingDefaultStaging || name == ".actual-staging" || name == ".git" || name == "node_modules" {
		return true
	}

	return strings.HasPrefix(name, ".")
}
