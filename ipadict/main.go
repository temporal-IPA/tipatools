// File path: tipatools/ipadict/main.go

// The command "ipadict" builds IPA pronunciation dictionaries from multiple
// sources.
//
// It uses the phonodict and seqparser packages to:
//   - scan Wiktionary / Wikipedia XML dumps for {{pron}} / {{API}} templates,
//   - load and merge pre-existing dictionaries from several formats, and
//   - export the resulting dictionary as text or gob.
//
// Wikipedia / Wiktionary is treated as a major, high-coverage source, but the
// tool can also layer additional dictionaries via --preload / --parse and
// merge modes.

package main

import (
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/temporal-IPA/tipa/pkg/phonodict"
	"github.com/temporal-IPA/tipa/pkg/phonodict/seqparser"
)

// --- CLI help / usage -------------------------------------------------------

const helpText = `ipadict - IPA pronunciation dictionary builder

Usage:
  ipadict help
      Print this help message.

  ipadict [flags] --parse <path-or-URL> [--parse <path-or-URL> ...]
      Build an IPA dictionary from one or more sources (Wiktionary /
      Wikipedia XML dumps, existing dictionaries, or a mix of both).

Sources:

  --parse PATH
      Add a source to the pipeline. This flag can be repeated; sources
      are processed in the order they appear on the command line.

      Each PATH can be:
        - a Wiktionary / Wikipedia XML dump:
            *.xml
            *.xml.bz2
            *wiktionary*.bz2
            *wikipedia*.bz2
          (local file or HTTP/HTTPS URL), or
        - a pre-existing dictionary:
            - native ipadict text:
                <word>\t<IPA1> | <IPA2> | ...
            - gob (map[string][]string) produced by "--export gob"
            - "ipa-dict" style slashed text:
                <word>\t/<IPA>/
                <word>\t/<IPA1>/ /<IPA2>/

  --preload PATH
      Preload an existing dictionary before any --parse sources.
      The flag can be repeated; dictionaries are merged in the order
      they are given. It accepts the same dictionary formats as
      --parse, but is always treated as a dictionary (never as a dump).

Flags:
  --lang CODE
      Language code to match in {{pron|...}} / {{API|...}} templates when
      scanning Wikimedia dumps.
      Default is "fr". Examples: "fr", "en", "es", "de".

  --export text
      Export a UTF-8 text dictionary to stdout (default).
      Format: one entry per line
          <word>\t<IPA1> | <IPA2> | ...
      Example:
          fauteuil  fo.tœj
          grand     gʁɑ̃ | gʁã

  --export gob
      Export a binary encoding (encoding/gob) of a map[string][]string to stdout.
      This is useful for fast re-loading inside Go tools.
      Example:
          ipadict --lang fr --export gob --parse dump.xml.bz2 > fr.dict.gob

  --preload PATH
      Preload an existing dictionary before any --parse sources.
      This flag can be used multiple times; dictionaries are preloaded
      and merged in the order they are given.

  --merge-append
      Merge new pronunciations into the existing dictionary by appending them
      after existing entries (default). New pronunciations for a word are added
      at the end of the existing list, with de-duplication on (word, pronunciation).

  --merge-prepend
      Merge new pronunciations by prepending them before existing entries for
      each word. This is useful when the newly parsed source should have higher
      priority than the preloaded dictionaries.

  --merge
      Alias for --merge-append (kept for backward compatibility).

  --no-override
      Do not change entries for words that already exist in the preloaded
      dictionaries. New pronunciations are only added for words that are not
      present in the preloaded set.

  --replace
      Replace entries for words that already exist in the preloaded
      dictionaries. As soon as a word appears in a new source, its existing
      pronunciations from the preloaded dictionaries are discarded and the
      new pronunciations become the reference set.

Input formats for --parse:
  - Local files:
      - Plain XML dumps:  *.xml
      - Bzip2-compressed: *.xml.bz2, *wiktionary*.bz2, *wikipedia*.bz2
      - Text or gob dictionaries as described above.
  - HTTP/HTTPS:
      When <path-or-URL> starts with "http://" or "https://", the dump is read
      directly from the HTTP response body as a stream. If the URL path ends
      with ".bz2" (e.g. Wikimedia dump URLs), the content is transparently
      decompressed on the fly without creating temporary files.

Examples:
  # Basic local scan (French, text export)
  ipadict --lang fr \
          --parse frwiktionary-latest-pages-articles.xml.bz2 \
          --export text \
          > exports/fr.dict.txt

  # English Wiktionary dictionary
  ipadict --lang en \
          --parse enwiktionary-latest-pages-articles.xml.bz2 \
          --export text \
          > exports/en.dict.txt

  # Explicit gob export
  ipadict --lang fr --export gob \
          --parse frwiktionary-latest-pages-articles.xml.bz2 \
          > exports/fr.dict.gob

  # Merge existing French dictionaries with a new dump (append new pronunciations)
  ipadict --lang fr --merge-append \
          --preload exports/old.dict.txt \
          --preload exports/custom.dict.txt \
          --parse frwiktionary-new-pages-articles.xml.bz2 \
          > exports/merged.dict.txt

  # Do not touch words that already exist in the preloaded dictionaries
  ipadict --lang fr --no-override \
          --preload exports/curated.dict.txt \
          --parse frwiktionary-latest-pages-articles.xml.bz2 \
          > exports/fr.curated_plus_missing.dict.txt

  # Replace existing entries with new pronunciations when available
  ipadict --lang fr --replace \
          --preload exports/old.dict.txt \
          --parse frwiktionary-20251120-pages-articles-multistream.xml.bz2 \
          > exports/fr.override_old.dict.txt

  # Single-pass build from a dump and an external ipa-dict text file
  ipadict --lang fr --merge-append \
          --parse frwiktionary-20251120-pages-articles-multistream.xml \
          --parse datasets/ipa-dict/fr_FR.txt \
          --export text \
          > exports/fr.full.dict.txt
`

// printUsage writes the CLI help text to the given writer.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, helpText)
}

// --- Dictionary export helpers ----------------------------------------------

// writeTextDictionary prints the dictionary as a sorted text list on w.
//
// Format:
//
//	<word>\t<IPA1> | <IPA2> | ...
func writeTextDictionary(w io.Writer, entries map[string][]string) error {
	words := make([]string, 0, len(entries))
	for word := range entries {
		words = append(words, word)
	}
	sort.Strings(words)

	for _, word := range words {
		prons := entries[word]
		if len(prons) == 0 {
			continue
		}
		line := fmt.Sprintf("%s\t%s\n", word, strings.Join(prons, " | "))
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	return nil
}

// writeGobDictionary encodes entries as a gob-encoded map[string][]string on w.
func writeGobDictionary(w io.Writer, entries map[string][]string) error {
	enc := gob.NewEncoder(w)
	return enc.Encode(entries)
}

// --- CLI wiring -------------------------------------------------------------

// buildConfig holds options for a full dictionary build.
type buildConfig struct {
	ParseSources []string            // sources passed via --parse (dumps or dictionaries)
	PreloadPaths []string            // sources passed via --preload (always dictionaries)
	ExportFormat string              // "text" or "gob"
	Lang         string              // language code used in pron/API templates
	MergeMode    phonodict.MergeMode // append, prepend, no-override, replace
}

// stringSliceFlag implements flag.Value to allow repeated flags.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// isHTTPURL returns true if src looks like an HTTP or HTTPS URL.
func isHTTPURL(src string) bool {
	return strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")
}

// isDumpSource classifies pathOrURL as a Wiktionary/Wikipedia XML dump
// when its shape strongly suggests it.
func isDumpSource(pathOrURL string) bool {
	if isHTTPURL(pathOrURL) {
		return true
	}

	lower := strings.ToLower(pathOrURL)

	if strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".xml.bz2") {
		return true
	}

	if strings.HasSuffix(lower, ".bz2") &&
		(strings.Contains(lower, "wiktionary") ||
			strings.Contains(lower, "wikipedia") ||
			strings.Contains(lower, "wikimedia")) {
		return true
	}

	return false
}

// runBuild executes a full build according to cfg and writes the result to stdout.
func runBuild(cfg buildConfig) error {
	if len(cfg.ParseSources) == 0 && len(cfg.PreloadPaths) == 0 {
		return errors.New("at least one --parse or --preload source must be specified")
	}

	export := strings.ToLower(strings.TrimSpace(cfg.ExportFormat))
	if export == "" {
		export = "text"
	}
	if export != "text" && export != "gob" {
		return fmt.Errorf("invalid --export value %q (must be \"text\" or \"gob\")", cfg.ExportFormat)
	}

	lang := strings.ToLower(strings.TrimSpace(cfg.Lang))
	if lang == "" {
		lang = "fr"
	}

	rep := phonodict.NewRepresentation()

	// Step 1: preload dictionaries (always treated as dictionaries).
	if len(cfg.PreloadPaths) > 0 {
		if err := phonodict.PreloadInto(rep, cfg.MergeMode, cfg.PreloadPaths...); err != nil {
			return fmt.Errorf("preload %q: %w", strings.Join(cfg.PreloadPaths, ", "), err)
		}
	}

	// Step 2: process --parse sources in order.
	var totalLines int
	var totalElapsed time.Duration

	for _, src := range cfg.ParseSources {
		src = strings.TrimSpace(src)
		if src == "" {
			continue
		}

		if isDumpSource(src) {
			parser := seqparser.NewXMLWikipediaDump(lang, cfg.MergeMode)
			parser.Progress = func(lines, words, uniquePairs int) {
				fmt.Fprintf(os.Stderr,
					"\rScanning %s... lines: %d (words: %d, unique word/pron pairs: %d)",
					src, lines, len(rep.Entries), len(rep.SeenWordPron))
			}

			stats, err := parser.ParseSource(src, rep)
			if err != nil {
				return fmt.Errorf("scan %q: %w", src, err)
			}

			totalLines += stats.Lines
			totalElapsed += stats.Elapsed

			fmt.Fprintf(os.Stderr,
				"\rFinished %s. Scanned lines: %d (words: %d, unique word/pron pairs: %d, elapsed: %.3f seconds)\n",
				src, stats.Lines, len(rep.Entries), len(rep.SeenWordPron), stats.Elapsed.Seconds())
		} else {
			// Treat as dictionary source, using phonodict preloaders.
			if err := phonodict.PreloadInto(rep, cfg.MergeMode, src); err != nil {
				return fmt.Errorf("preload %q: %w", src, err)
			}
		}
	}

	// Step 3: export dictionary.
	switch export {
	case "text":
		if err := writeTextDictionary(os.Stdout, rep.Entries); err != nil {
			return fmt.Errorf("write text: %w", err)
		}
	case "gob":
		if err := writeGobDictionary(os.Stdout, rep.Entries); err != nil {
			return fmt.Errorf("write gob: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr,
		"Finished. Scanned lines: %d (words: %d, unique word/pron pairs: %d, total elapsed: %.3f seconds)\n",
		totalLines, len(rep.Entries), len(rep.SeenWordPron), totalElapsed.Seconds())

	return nil
}

// runFromArgs parses flags/positional arguments and delegates to runBuild.
func runFromArgs(args []string) error {
	fs := flag.NewFlagSet("ipadict", flag.ContinueOnError)

	exportFormat := fs.String("export", "text", "export format: text or gob")

	var parseSources stringSliceFlag
	fs.Var(&parseSources, "parse", "source to parse (dump or dictionary). Can be repeated; order matters.")

	var preloadPaths stringSliceFlag
	fs.Var(&preloadPaths, "preload", "dictionary to preload before any --parse sources (text, gob, ipa_dict_txt). Can be repeated.")

	lang := fs.String("lang", "fr", "language code to match in pron/API templates (e.g. fr, en, es, de)")

	mergeFlag := fs.Bool("merge", false, "alias for --merge-append (merge new pronunciations by appending them)")
	mergeAppendFlag := fs.Bool("merge-append", false, "merge new pronunciations into existing entries by appending them (default)")
	mergePrependFlag := fs.Bool("merge-prepend", false, "merge new pronunciations by prepending them before existing entries")

	noOverrideFlag := fs.Bool("no-override", false, "do not change entries for words that already exist in the preloaded dictionary")
	noOverrideCompat := fs.Bool("no-overide", false, "alias for --no-override")
	replaceFlag := fs.Bool("replace", false, "replace entries for words that already exist in the preloaded dictionary")

	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		printUsage(os.Stderr)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return nil
		}
		return err
	}

	mode := phonodict.MergeModeAppend
	selected := 0

	if *mergeFlag || *mergeAppendFlag {
		mode = phonodict.MergeModeAppend
		selected++
	}
	if *mergePrependFlag {
		mode = phonodict.MergeModePrepend
		selected++
	}
	if *noOverrideFlag || *noOverrideCompat {
		mode = phonodict.MergeModeNoOverride
		selected++
	}
	if *replaceFlag {
		mode = phonodict.MergeModeReplace
		selected++
	}

	if selected > 1 {
		return errors.New("only one of --merge/--merge-append, --merge-prepend, --no-override/--no-overide, or --replace may be specified")
	}

	cfg := buildConfig{
		ParseSources: parseSources,
		PreloadPaths: preloadPaths,
		ExportFormat: strings.TrimSpace(*exportFormat),
		Lang:         strings.TrimSpace(*lang),
		MergeMode:    mode,
	}

	return runBuild(cfg)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "-h", "--help":
			printUsage(os.Stdout)
			return
		}
	}

	if err := runFromArgs(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
