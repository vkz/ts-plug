package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const maxTaildropRenameAttempts = 10000

const taildropSocketAndACLNotes = `SOCKET RESOLUTION
  The daemon control socket is located using the first non-empty source:
    1. -socket <path>
    2. <dir>/control.sock (from -dir <dir>)
    3. $TS_UNPLUG_PROXY_SOCK
    4. $TS_UNPLUG_PROXY_DIR/control.sock
    5. $XDG_STATE_HOME/ts-unplug-proxy/control.sock (or ~/.local/state/ts-unplug-proxy/control.sock)

TAILDROP AND ACL NOTES
  Taildrop is enabled by default between user-owned (untagged) nodes belonging
  to the same user. If the daemon is running as a tagged node via
  -advertise-tags, Taildrop requires explicit Tailscale ACL file-sharing
  capabilities/grants in your tailnet policy to send or receive files.
`

const fileUsageText = `ts-unplug-proxy file - send and receive files over Taildrop via a running
ts-unplug-proxy daemon's Unix control socket.

USAGE
  ts-unplug-proxy file <subcommand> [options] [args...]
  ts-unplug-proxy cp [options] <file...> <peer>:

SUBCOMMANDS
  cp       Send one or more files (or stdin '-') to a tailnet peer
  get      Download waiting Taildrop files from the daemon to a directory
  ls       List waiting Taildrop files held by the daemon
  targets  List tailnet peers eligible to receive Taildrop files

  Run 'ts-unplug-proxy file <subcommand> -h' for detailed flags and examples.

EXAMPLES
  # List peers that can receive files
  ts-unplug-proxy file targets

  # Send files to a peer (note the trailing ':')
  ts-unplug-proxy file cp report.pdf laptop:
  ts-unplug-proxy cp report.pdf laptop:

  # Pipe stdin with an explicit filename
  echo "hello" | ts-unplug-proxy file cp -name note.txt - laptop:

  # List and retrieve incoming files
  ts-unplug-proxy file ls
  ts-unplug-proxy file get ~/Downloads

` + taildropSocketAndACLNotes

const fileCPUsageText = `ts-unplug-proxy file cp - send one or more files to a tailnet peer via Taildrop.

USAGE
  ts-unplug-proxy file cp [-dir <dir>] [-socket <path>] [-name <filename>] <file...> <peer>:

  The destination peer MUST end with a trailing colon ':' (matching
  'tailscale file cp'). <peer> may be a peer's short name, hostname, MagicDNS
  FQDN (with or without trailing '.'), or Tailscale IP address (wrap IPv6
  literals in brackets, e.g. [fd7a:115c:a1e0::1]:).

  Pass '-' as the single source file to stream from stdin; -name <filename> is
  required when reading from stdin.

OPTIONS
  -dir <dir>         ts-unplug-proxy state directory containing control.sock
  -socket <path>     explicit path to the daemon Unix control socket
  -name <filename>   override destination filename (single source file only;
                     required when source is '-' for stdin)

EXAMPLES
  # Send a single file by short peer name
  ts-unplug-proxy file cp report.pdf laptop:

  # Send multiple files by MagicDNS name or Tailscale IP
  ts-unplug-proxy file cp a.txt b.txt laptop.tailnet.ts.net:
  ts-unplug-proxy file cp archive.tar.gz 100.64.0.12:

  # Rename while sending, or send from stdin
  ts-unplug-proxy file cp -name final.pdf draft.pdf laptop:
  tar czf - ./project | ts-unplug-proxy file cp -name project.tgz - laptop:

` + taildropSocketAndACLNotes

const cpUsageText = `ts-unplug-proxy cp - shorthand for 'ts-unplug-proxy file cp'.

USAGE
  ts-unplug-proxy cp [-dir <dir>] [-socket <path>] [-name <filename>] <file...> <peer>:

  This command is a direct alias for 'ts-unplug-proxy file cp'. For receiving
  and inspecting Taildrop files and targets, see:
    ts-unplug-proxy file get -h      download waiting incoming files
    ts-unplug-proxy file ls -h       list waiting incoming files
    ts-unplug-proxy file targets -h  list eligible destination peers

  The destination peer MUST end with a trailing colon ':' (e.g. laptop:).
  Pass '-' as the single source file with -name <filename> to stream stdin.

OPTIONS
  -dir <dir>         ts-unplug-proxy state directory containing control.sock
  -socket <path>     explicit path to the daemon Unix control socket
  -name <filename>   override destination filename (single source file only;
                     required when source is '-' for stdin)

EXAMPLES
  # Send a file to a peer
  ts-unplug-proxy cp report.pdf laptop:

  # Send multiple files using an explicit state directory
  ts-unplug-proxy cp -dir ~/.local/state/ts-unplug-proxy a.txt b.txt laptop:

  # Stream stdin to a peer
  echo "hello" | ts-unplug-proxy cp -name hello.txt - laptop:

` + taildropSocketAndACLNotes

const fileGetUsageText = `ts-unplug-proxy file get - download waiting Taildrop files from the daemon.

USAGE
  ts-unplug-proxy file get [-dir <dir>] [-socket <path>] [-conflict skip|rename|overwrite] [-keep] [dest-dir]

  Downloads files received by the running ts-unplug-proxy daemon into dest-dir
  (default '.'). Each file is streamed to a temporary file in dest-dir, synced
  to disk, and atomically placed before being removed from the daemon (unless
  -keep is specified). Unsafe filenames containing path separators or traversal
  components are always refused.

OPTIONS
  -dir <dir>         ts-unplug-proxy state directory containing control.sock
  -socket <path>     explicit path to the daemon Unix control socket
  -conflict <mode>   policy when destination file already exists (default "skip"):
                       skip      leave existing file untouched and keep file in daemon
                       rename    save as stem-1.ext, stem-2.ext, ...
                       overwrite atomically replace the existing file
  -keep              do not delete files from the daemon after downloading

EXAMPLES
  # Download waiting files into the current directory (skip existing)
  ts-unplug-proxy file get

  # Download into ~/Downloads, renaming on collision
  ts-unplug-proxy file get -conflict rename ~/Downloads

  # Overwrite existing files while keeping copies in the daemon
  ts-unplug-proxy file get -conflict overwrite -keep ./inbox

` + taildropSocketAndACLNotes

const fileLSUsageText = `ts-unplug-proxy file ls - list waiting Taildrop files held by the daemon.

USAGE
  ts-unplug-proxy file ls [-json] [-dir <dir>] [-socket <path>]

OPTIONS
  -json              output waiting files as a pretty-printed JSON array
  -dir <dir>         ts-unplug-proxy state directory containing control.sock
  -socket <path>     explicit path to the daemon Unix control socket

EXAMPLES
  # List waiting incoming files
  ts-unplug-proxy file ls

  # Machine-readable JSON output
  ts-unplug-proxy file ls -json

  # Query a daemon using a specific state directory
  ts-unplug-proxy file ls -dir ~/.local/state/ts-unplug-proxy

` + taildropSocketAndACLNotes

const fileTargetsUsageText = `ts-unplug-proxy file targets - list tailnet peers eligible for Taildrop.

USAGE
  ts-unplug-proxy file targets [-json] [-dir <dir>] [-socket <path>]

OPTIONS
  -json              output targets as a pretty-printed JSON array
  -dir <dir>         ts-unplug-proxy state directory containing control.sock
  -socket <path>     explicit path to the daemon Unix control socket

EXAMPLES
  # Show available Taildrop target peers
  ts-unplug-proxy file targets

  # Machine-readable JSON output
  ts-unplug-proxy file targets -json

  # Query a daemon using an explicit control socket path
  ts-unplug-proxy file targets -socket /tmp/ts-unplug/control.sock

` + taildropSocketAndACLNotes

// runFile dispatches 'ts-unplug-proxy file <cp|get|ls|targets>'.
func runFile(args []string) int {
	return runFileIO(args, os.Stdin, os.Stdout, os.Stderr)
}

func runFileIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, fileUsageText)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		_, _ = io.WriteString(stderr, fileUsageText)
		return 0
	case "cp":
		return runFileCPIO(args[1:], stdin, stdout, stderr, "file cp", fileCPUsageText)
	case "get":
		return runFileGetIO(args[1:], stdout, stderr)
	case "ls":
		return runFileLSIO(args[1:], stdout, stderr)
	case "targets":
		return runFileTargetsIO(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ts-unplug-proxy file: unknown subcommand %q (expected cp, get, ls, or targets)\n", args[0])
		fmt.Fprintln(stderr, "Run 'ts-unplug-proxy file -h' for usage.")
		return 2
	}
}

// runCP is a direct shorthand alias for 'ts-unplug-proxy file cp'.
func runCP(args []string) int {
	return runCPIO(args, os.Stdin, os.Stdout, os.Stderr)
}

func runCPIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runFileCPIO(args, stdin, stdout, stderr, "cp", cpUsageText)
}

func runFileCPIO(args []string, stdin io.Reader, stdout, stderr io.Writer, cmdName, usage string) int {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, usage)
	}

	dirFlag := fs.String("dir", "", "ts-unplug-proxy state directory containing control.sock")
	sockFlag := fs.String("socket", "", "explicit path to the daemon Unix control socket")
	nameFlag := fs.String("name", "", "override destination filename (single source file only; required for '-')")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	pos := fs.Args()
	if len(pos) < 2 {
		fmt.Fprintf(stderr, "ts-unplug-proxy %s: expected <file...> <peer>:\n", cmdName)
		fmt.Fprintf(stderr, "Run 'ts-unplug-proxy %s -h' for usage.\n", cmdName)
		return 2
	}

	files := pos[:len(pos)-1]
	rawDest := pos[len(pos)-1]
	peerQuery, ok := strings.CutSuffix(rawDest, ":")
	if !ok {
		fmt.Fprintf(stderr, "ts-unplug-proxy %s: destination must end with ':' (e.g. peer:)\n", cmdName)
		return 2
	}
	peerQuery = strings.TrimSpace(peerQuery)
	if peerQuery == "" {
		fmt.Fprintf(stderr, "ts-unplug-proxy %s: destination peer name must not be empty\n", cmdName)
		return 2
	}
	if strings.HasPrefix(peerQuery, "[") && strings.HasSuffix(peerQuery, "]") {
		peerQuery = strings.TrimSuffix(strings.TrimPrefix(peerQuery, "["), "]")
		if peerQuery == "" {
			fmt.Fprintf(stderr, "ts-unplug-proxy %s: destination peer address must not be empty\n", cmdName)
			return 2
		}
	}

	if *nameFlag != "" {
		if len(files) > 1 {
			fmt.Fprintf(stderr, "ts-unplug-proxy %s: -name can only be used with a single source file\n", cmdName)
			return 2
		}
		if _, err := sanitizeTaildropName(*nameFlag); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy %s: invalid -name %q: %v\n", cmdName, *nameFlag, err)
			return 2
		}
	}

	for _, f := range files {
		if f == "-" {
			if len(files) > 1 {
				fmt.Fprintf(stderr, "ts-unplug-proxy %s: cannot combine '-' (stdin) with multiple source files\n", cmdName)
				return 2
			}
			if *nameFlag == "" {
				fmt.Fprintf(stderr, "ts-unplug-proxy %s: -name <filename> is required when reading from stdin ('-')\n", cmdName)
				return 2
			}
		}
	}

	// Pre-validate all local file sources before transferring anything so a
	// missing path or directory later in the argument list fails cleanly
	// without partial transfers.
	for _, src := range files {
		if src == "-" {
			continue
		}
		fi, err := os.Stat(src)
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy %s: %v\n", cmdName, err)
			return 1
		}
		if fi.IsDir() {
			fmt.Fprintf(stderr, "ts-unplug-proxy %s: cannot send %q: directories are not supported\n", cmdName, src)
			return 1
		}
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dirFlag)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy %s: %v\n", cmdName, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lc := controlLocalClient(sockPath)
	targets, err := lc.FileTargets(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy %s: list file targets: %v\n", cmdName, formatLocalAPIError(sockPath, err))
		return 1
	}

	target, err := resolveFileTarget(targets, peerQuery)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy %s: %v\n", cmdName, err)
		return 1
	}
	targetLabel := fileTargetShortName(target.Node)

	for _, src := range files {
		if src == "-" {
			cr := &byteCountingReader{r: stdin}
			if err := lc.PushFile(ctx, target.Node.StableID, -1, *nameFlag, cr); err != nil {
				fmt.Fprintf(stderr, "ts-unplug-proxy %s: send %q to %s: %v\n", cmdName, *nameFlag, targetLabel, formatLocalAPIError(sockPath, err))
				return 1
			}
			fmt.Fprintf(stderr, "Sent %s (%s) to %s\n", *nameFlag, formatByteSize(cr.n), targetLabel)
			continue
		}

		sentName, sentSize, err := pushRegularFile(ctx, lc, target.Node.StableID, src, *nameFlag)
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy %s: %v\n", cmdName, formatLocalAPIError(sockPath, err))
			return 1
		}
		fmt.Fprintf(stderr, "Sent %s (%s) to %s\n", sentName, formatByteSize(sentSize), targetLabel)
	}

	return 0
}

type filePusher interface {
	PushFile(ctx context.Context, target tailcfg.StableNodeID, size int64, name string, r io.Reader) error
}

func pushRegularFile(ctx context.Context, pusher filePusher, targetID tailcfg.StableNodeID, srcPath, overrideName string) (string, int64, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if fi.IsDir() {
		return "", 0, fmt.Errorf("cannot send directory %q: directories are not supported", srcPath)
	}

	baseName := overrideName
	if baseName == "" {
		baseName = filepath.Base(srcPath)
	}
	if _, err := sanitizeTaildropName(baseName); err != nil {
		return "", 0, fmt.Errorf("invalid filename %q: %v", baseName, err)
	}

	var size int64 = -1
	var body io.Reader = f
	if fi.Mode().IsRegular() {
		size = fi.Size()
		body = io.LimitReader(f, size)
	}
	cr := &byteCountingReader{r: body}

	if err := pusher.PushFile(ctx, targetID, size, baseName, cr); err != nil {
		return "", 0, fmt.Errorf("send %q: %w", baseName, err)
	}
	if size >= 0 {
		return baseName, size, nil
	}
	return baseName, cr.n, nil
}

type byteCountingReader struct {
	r io.Reader
	n int64
}

func (c *byteCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func runFileGetIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("file get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, fileGetUsageText)
	}

	dirFlag := fs.String("dir", "", "ts-unplug-proxy state directory containing control.sock")
	sockFlag := fs.String("socket", "", "explicit path to the daemon Unix control socket")
	conflictFlag := fs.String("conflict", "skip", "conflict policy: skip, rename, or overwrite")
	keepFlag := fs.Bool("keep", false, "keep files in the daemon after downloading")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	conflict := strings.ToLower(strings.TrimSpace(*conflictFlag))
	switch conflict {
	case "skip", "rename", "overwrite":
	default:
		fmt.Fprintf(stderr, "ts-unplug-proxy file get: invalid -conflict %q (must be skip, rename, or overwrite)\n", *conflictFlag)
		return 2
	}

	destDir := "."
	switch fs.NArg() {
	case 0:
	case 1:
		destDir = fs.Arg(0)
	default:
		fmt.Fprintln(stderr, "ts-unplug-proxy file get: expected at most one destination directory")
		return 2
	}

	fi, err := os.Stat(destDir)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file get: destination directory %q: %v\n", destDir, err)
		return 1
	}
	if !fi.IsDir() {
		fmt.Fprintf(stderr, "ts-unplug-proxy file get: destination %q is not a directory\n", destDir)
		return 1
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dirFlag)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file get: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lc := controlLocalClient(sockPath)
	waiting, err := lc.WaitingFiles(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file get: list waiting files: %v\n", formatLocalAPIError(sockPath, err))
		return 1
	}
	if len(waiting) == 0 {
		fmt.Fprintln(stdout, "No waiting files.")
		return 0
	}

	hadErr := false
	cleanDestDir := filepath.Clean(destDir)

	for _, wf := range waiting {
		safeName, err := sanitizeTaildropName(wf.Name)
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy file get: refusing unsafe filename %q: %v\n", wf.Name, err)
			hadErr = true
			continue
		}

		destPath := filepath.Join(cleanDestDir, safeName)
		if filepath.Dir(destPath) != cleanDestDir {
			fmt.Fprintf(stderr, "ts-unplug-proxy file get: refusing filename %q resolving outside %q\n", wf.Name, destDir)
			hadErr = true
			continue
		}

		// Fast path for skip: avoid downloading if target already exists.
		if conflict == "skip" {
			if _, statErr := os.Lstat(destPath); statErr == nil {
				fmt.Fprintf(stderr, "Skipping %s (already exists)\n", safeName)
				continue
			} else if !os.IsNotExist(statErr) {
				fmt.Fprintf(stderr, "ts-unplug-proxy file get: check %s: %v\n", destPath, statErr)
				hadErr = true
				continue
			}
		}

		rc, _, err := lc.GetWaitingFile(ctx, wf.Name)
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy file get: download %q: %v\n", safeName, formatLocalAPIError(sockPath, err))
			hadErr = true
			continue
		}

		tmpPath, written, err := writeTempFileDurable(cleanDestDir, rc)
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy file get: write %q: %v\n", safeName, err)
			hadErr = true
			continue
		}

		finalName, skipped, err := commitTaildropFile(tmpPath, cleanDestDir, safeName, conflict)
		if err != nil {
			_ = os.Remove(tmpPath)
			fmt.Fprintf(stderr, "ts-unplug-proxy file get: save %q: %v\n", safeName, err)
			hadErr = true
			continue
		}
		if skipped {
			_ = os.Remove(tmpPath)
			fmt.Fprintf(stderr, "Skipping %s (already exists)\n", safeName)
			continue
		}

		// Sync the destination directory so the new directory entry is durable
		// before removing the staged copy from the daemon.
		if err := syncDir(cleanDestDir); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy file get: saved %s, but failed to sync destination directory (leaving staged file in daemon): %v\n", finalName, err)
			hadErr = true
			continue
		}

		if !*keepFlag {
			if err := lc.DeleteWaitingFile(ctx, wf.Name); err != nil {
				fmt.Fprintf(stderr, "ts-unplug-proxy file get: saved %s, but failed to delete from daemon: %v\n", finalName, formatLocalAPIError(sockPath, err))
				hadErr = true
				continue
			}
		}

		fmt.Fprintf(stdout, "Received %s (%s)\n", finalName, formatByteSize(written))
	}

	if hadErr {
		return 1
	}
	return 0
}

// writeTempFileDurable streams rc into a new temporary file in destDir, syncs
// it to disk, and closes it. On any error, the temporary file is removed.
func writeTempFileDurable(destDir string, rc io.ReadCloser) (tmpPath string, written int64, err error) {
	defer rc.Close()

	tmp, err := os.CreateTemp(destDir, ".taildrop-*")
	if err != nil {
		return "", 0, err
	}
	tmpPath = tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	written, err = io.Copy(tmp, rc)
	if err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}

	cleanup = false
	return tmpPath, written, nil
}

// commitTaildropFile places tmpPath into destDir according to conflict policy.
// For "skip" and "rename" it avoids clobbering existing files by linking
// atomically (with a safe fallback if hard links are unsupported).
func commitTaildropFile(tmpPath, destDir, safeName, conflict string) (finalName string, skipped bool, err error) {
	destPath := filepath.Join(destDir, safeName)

	switch conflict {
	case "overwrite":
		if err := os.Rename(tmpPath, destPath); err != nil {
			return "", false, err
		}
		return safeName, false, nil

	case "skip":
		if err := placeFileExclusive(tmpPath, destPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				return "", true, nil
			}
			return "", false, err
		}
		return safeName, false, nil

	case "rename":
		for i := 0; i < maxTaildropRenameAttempts; i++ {
			candName := renameCandidate(safeName, i)
			candPath := filepath.Join(destDir, candName)
			if err := placeFileExclusive(tmpPath, candPath); err == nil {
				return candName, false, nil
			} else if !errors.Is(err, os.ErrExist) {
				return "", false, err
			}
		}
		return "", false, fmt.Errorf("could not find unused filename for %q after %d attempts", safeName, maxTaildropRenameAttempts)

	default:
		return "", false, fmt.Errorf("unsupported conflict policy %q", conflict)
	}
}

// syncDir flushes the directory entry metadata to disk on platforms/filesystems
// that support fsyncing directories. Unsupported-operation errors (for example
// on Windows or virtual filesystems) are ignored.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP) || errors.Is(syncErr, syscall.EPERM) || errors.Is(syncErr, syscall.EBADF) {
			return closeErr
		}
		return syncErr
	}
	return closeErr
}

// placeFileExclusive atomically moves tmpPath to destPath only if destPath does
// not already exist. Returns os.ErrExist when destPath already exists.
func placeFileExclusive(tmpPath, destPath string) error {
	err := os.Link(tmpPath, destPath)
	if err == nil {
		_ = os.Remove(tmpPath)
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return os.ErrExist
	}
	// Fallback for filesystems that do not support hard links (e.g. FAT32/FUSE).
	if _, statErr := os.Lstat(destPath); statErr == nil {
		return os.ErrExist
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	return os.Rename(tmpPath, destPath)
}

func renameCandidate(name string, attempt int) string {
	if attempt == 0 {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	// Hidden file with a single leading dot (e.g. ".env"): keep dot as stem.
	if stem == "" {
		stem = name
		ext = ""
	}
	return fmt.Sprintf("%s-%d%s", stem, attempt, ext)
}

func runFileLSIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("file ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, fileLSUsageText)
	}

	jsonFlag := fs.Bool("json", false, "output waiting files as pretty-printed JSON")
	dirFlag := fs.String("dir", "", "ts-unplug-proxy state directory containing control.sock")
	sockFlag := fs.String("socket", "", "explicit path to the daemon Unix control socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "ts-unplug-proxy file ls: unexpected positional arguments")
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dirFlag)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file ls: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lc := controlLocalClient(sockPath)
	waiting, err := lc.WaitingFiles(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file ls: %v\n", formatLocalAPIError(sockPath, err))
		return 1
	}

	if *jsonFlag {
		if waiting == nil {
			waiting = []apitype.WaitingFile{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(waiting); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy file ls: encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	if len(waiting) == 0 {
		fmt.Fprintln(stdout, "No waiting files.")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE")
	for _, wf := range waiting {
		fmt.Fprintf(tw, "%s\t%s\n", wf.Name, formatByteSize(wf.Size))
	}
	_ = tw.Flush()
	return 0
}

func runFileTargetsIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("file targets", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, fileTargetsUsageText)
	}

	jsonFlag := fs.Bool("json", false, "output targets as pretty-printed JSON")
	dirFlag := fs.String("dir", "", "ts-unplug-proxy state directory containing control.sock")
	sockFlag := fs.String("socket", "", "explicit path to the daemon Unix control socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "ts-unplug-proxy file targets: unexpected positional arguments")
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dirFlag)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file targets: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lc := controlLocalClient(sockPath)
	targets, err := lc.FileTargets(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy file targets: %v\n", formatLocalAPIError(sockPath, err))
		return 1
	}

	if *jsonFlag {
		if targets == nil {
			targets = []apitype.FileTarget{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(targets); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy file targets: encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	if len(targets) == 0 {
		fmt.Fprintln(stdout, "No Taildrop targets available.")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDNS\tIP")
	for _, ft := range targets {
		if ft.Node == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			fileTargetShortName(ft.Node),
			fileTargetDNS(ft.Node),
			fileTargetIP(ft.Node),
		)
	}
	_ = tw.Flush()
	return 0
}

// sanitizeTaildropName validates that name is a single, safe base filename with
// no path separators, NUL bytes, drive/volume prefixes, or traversal entries.
func sanitizeTaildropName(name string) (string, error) {
	if name == "" {
		return "", errors.New("filename must not be empty")
	}
	if name == "." || name == ".." {
		return "", errors.New("filename must not be '.' or '..'")
	}
	if strings.ContainsRune(name, '\x00') {
		return "", errors.New("filename contains NUL byte")
	}
	if strings.ContainsAny(name, `/\`) {
		return "", errors.New("filename must not contain path separators")
	}
	if filepath.VolumeName(name) != "" {
		return "", errors.New("filename must not contain a volume/drive prefix")
	}
	if filepath.Base(name) != name || filepath.Clean(name) != name {
		return "", errors.New("filename must be a clean base filename")
	}
	return name, nil
}

// resolveFileTarget matches query against targets case-insensitively by:
//   - exact Tailscale IP address (ft.Node.Addresses)
//   - exact MagicDNS FQDN ft.Node.Name (with or without trailing '.')
//   - exact hostname (ft.Node.ComputedName or ft.Node.Hostinfo.Hostname())
//   - short prefix before first '.' of ft.Node.Name
func resolveFileTarget(targets []apitype.FileTarget, query string) (apitype.FileTarget, error) {
	var validTargets []apitype.FileTarget
	for _, ft := range targets {
		if ft.Node != nil {
			validTargets = append(validTargets, ft)
		}
	}
	if len(validTargets) == 0 {
		return apitype.FileTarget{}, errors.New("no Taildrop targets available (Taildrop requires user-owned untagged nodes or Tailscale ACL file-sharing capabilities, and at least one online peer)")
	}

	q := strings.TrimSpace(query)
	qNoDot := strings.TrimSuffix(q, ".")
	qAddr, isIP := parseAddrMaybe(q)

	var matches []apitype.FileTarget
	for _, ft := range validTargets {
		if fileTargetMatches(ft.Node, q, qNoDot, qAddr, isIP) {
			matches = append(matches, ft)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		avail := make([]string, 0, len(validTargets))
		for _, ft := range validTargets {
			avail = append(avail, fileTargetShortName(ft.Node))
		}
		return apitype.FileTarget{}, fmt.Errorf("peer %q not found in Taildrop targets; available targets: %s", query, strings.Join(avail, ", "))
	default:
		desc := make([]string, 0, len(matches))
		for _, ft := range matches {
			desc = append(desc, formatTargetBrief(ft.Node))
		}
		return apitype.FileTarget{}, fmt.Errorf("peer %q is ambiguous; matches: %s", query, strings.Join(desc, ", "))
	}
}

func parseAddrMaybe(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func fileTargetMatches(n *tailcfg.Node, q, qNoDot string, qAddr netip.Addr, isIP bool) bool {
	if isIP {
		for _, pfx := range n.Addresses {
			if pfx.Addr().Unmap() == qAddr {
				return true
			}
		}
	}

	dns := fileTargetDNS(n)
	if dns != "" && strings.EqualFold(qNoDot, dns) {
		return true
	}

	if n.ComputedName != "" && strings.EqualFold(q, n.ComputedName) {
		return true
	}
	if n.Hostinfo.Valid() {
		if hn := n.Hostinfo.Hostname(); hn != "" && strings.EqualFold(q, hn) {
			return true
		}
	}

	if dns != "" {
		short, _, _ := strings.Cut(dns, ".")
		if short != "" && strings.EqualFold(q, short) {
			return true
		}
	}
	return false
}

func fileTargetDNS(n *tailcfg.Node) string {
	if n == nil {
		return ""
	}
	return strings.TrimSuffix(n.Name, ".")
}

func fileTargetShortName(n *tailcfg.Node) string {
	if n == nil {
		return ""
	}
	if n.ComputedName != "" {
		return n.ComputedName
	}
	if n.Hostinfo.Valid() {
		if hn := n.Hostinfo.Hostname(); hn != "" {
			return hn
		}
	}
	if dns := fileTargetDNS(n); dns != "" {
		short, _, _ := strings.Cut(dns, ".")
		if short != "" {
			return short
		}
		return dns
	}
	return string(n.StableID)
}

func fileTargetIP(n *tailcfg.Node) string {
	if n == nil || len(n.Addresses) == 0 {
		return ""
	}
	// Prefer IPv4 address for readability, falling back to first address.
	for _, pfx := range n.Addresses {
		if pfx.Addr().Is4() {
			return pfx.Addr().String()
		}
	}
	return n.Addresses[0].Addr().String()
}

func formatTargetBrief(n *tailcfg.Node) string {
	name := fileTargetShortName(n)
	dns := fileTargetDNS(n)
	ip := fileTargetIP(n)
	switch {
	case dns != "" && ip != "":
		return fmt.Sprintf("%s (%s, %s)", name, dns, ip)
	case dns != "":
		return fmt.Sprintf("%s (%s)", name, dns)
	case ip != "":
		return fmt.Sprintf("%s (%s)", name, ip)
	default:
		return name
	}
}

func formatByteSize(n int64) string {
	if n < 0 {
		return fmt.Sprintf("%d B", n)
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n < kib:
		return fmt.Sprintf("%d B", n)
	case n < mib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	case n < gib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
}

func formatLocalAPIError(sockPath string, err error) error {
	if err == nil {
		return nil
	}
	var opErr *net.OpError
	if errors.Is(err, os.ErrNotExist) || errors.As(err, &opErr) {
		return fmt.Errorf("connect to control socket %s (is ts-unplug-proxy running?): %w", sockPath, err)
	}
	return err
}
