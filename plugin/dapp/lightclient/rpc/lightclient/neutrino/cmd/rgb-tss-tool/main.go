package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const usageText = `rgb-tss-tool exports and imports the CGGMP key material of one node
from a neutrino.db (bucket "rgbx-tss"), so backups do not need the whole
multi-GB SPV database.

Usage:
  rgb-tss-tool export --db <neutrino.db> --out <file> [--chain-title <title>]
  rgb-tss-tool import --file <file> --db <neutrino.db> [--force] [--chain-title <title>]

Commands:
  export   Write the rgbx-tss bucket to a 0600 JSON archive.
  import   Restore an archive into a neutrino.db. Refuses to overwrite existing
           key material unless --force is set; with --force the previous values
           are first copied to <db>.tss-backup-<unix>.

The tool must run while the node is stopped: bbolt allows a single writer.

The exported material is one node's share. A working signing group needs at
least threshold shares (currently 3 of 4); one archive alone cannot sign.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "export":
		return runExport(args[1:], stdout, stderr)
	case "import":
		return runImport(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "rgb-tss-tool: unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return 2
	}
}

func runExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to the node's neutrino.db (must exist)")
	outPath := fs.String("out", "", "archive file to write (0600)")
	chainTitle := fs.String("chain-title", "", "optional chain title recorded in the archive metadata")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dbPath == "" || *outPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "rgb-tss-tool export: --db and --out are required")
		fs.Usage()
		return 2
	}

	summary, err := exportTSS(exportOptions{
		DBPath:     *dbPath,
		OutPath:    *outPath,
		ChainTitle: *chainTitle,
		Warn:       stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "rgb-tss-tool export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "exported %d entries to %s\n", summary.Entries, summary.OutPath)
	fmt.Fprintf(stdout, "group pubkey: %s\n", summary.GroupPubKey)
	fmt.Fprintf(stdout, "rid: %s\n", summary.RID)
	fmt.Fprintf(stdout, "share id: %s\n", displayOrUnknown(summary.ShareID))
	return 0
}

func runImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	filePath := fs.String("file", "", "archive file to import")
	dbPath := fs.String("db", "", "path to the target neutrino.db (created when absent)")
	force := fs.Bool("force", false, "overwrite existing key material in the target database")
	chainTitle := fs.String("chain-title", "", "optional chain title checked against the archive metadata")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *filePath == "" || *dbPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "rgb-tss-tool import: --file and --db are required")
		fs.Usage()
		return 2
	}

	summary, err := importTSS(importOptions{
		FilePath:   *filePath,
		DBPath:     *dbPath,
		Force:      *force,
		ChainTitle: *chainTitle,
		Warn:       stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "rgb-tss-tool import: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "imported %d entries into %s\n", summary.Entries, summary.DBPath)
	fmt.Fprintf(stdout, "group pubkey: %s\n", summary.GroupPubKey)
	fmt.Fprintf(stdout, "rid: %s\n", summary.RID)
	fmt.Fprintf(stdout, "share id: %s\n", displayOrUnknown(summary.ShareID))
	fmt.Fprintln(stdout, "verified: stored values match the archive byte-for-byte")
	if summary.BackupPath != "" {
		fmt.Fprintf(stdout, "previous values backed up to %s\n", summary.BackupPath)
	}
	return 0
}

func displayOrUnknown(s string) string {
	if s == "" {
		return "unknown (not derivable from this DKG result)"
	}
	return s
}
