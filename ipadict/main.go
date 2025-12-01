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
// tool can also layer additional dictionaries via --preload and merge modes.

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

	"github.com/temporal-IPA/tipa/pkg/phonodict"
	"github.com/temporal-IPA/tipa/pkg/phonodict/seqparser"
)

// --- CLI help / usage -------------------------------------------------------

const helpText = `ipadict - IPA pronunciation dictionary builder

Usage:
  ipadict help
      Print this help message.

  ipadict parse [flags] <path-or-URL>
      Build an IPA dictionary from a primary source (by default: a
      Wiktionary / Wikipedia XML dump) and optional preloaded dictionaries.

Flags for "parse":
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
          ipadict parse --export gob dump.xml.bz2 > fr.dict.gob

  --preload PATH
      Preload an existing dictionary before scanning <path-or-URL>.
      This flag can be used multiple times; dictionaries are preloaded
      and merged in the order they are given.
      PATH can be:
        - a text dictionary produced by this tool (ipa_text):
            <word>\t<IPA1> | <IPA2> | ...
        - a gob file produced by "ipadict parse --export gob" (ipa_gob), or
        - an external text dictionary using "ipa_dict_txt" encoding:
            <word>\t/<IPA>/
            <word>\t/<IPA1>/ /<IPA2>/ ...

      The loader automatically sniffs the format from the first few
      kilobytes of each file and converts it to the internal
      representation before merging.

  --merge-append
      Merge new pronunciations into the existing dictionary by appending them
      after existing entries (default). New pronunciations for a word are added
      at the end of the existing list, with de-duplication on (word, pronunciation).

  --merge-prepend
      Merge new pronunciations by prepending them before existing entries for
      each word. This is useful when the newly parsed dump should have higher
      priority than the preloaded dictionaries.

  --merge
      Alias for --merge-append (kept for backward compatibility).

  --no-override
      Do not change entries for words that already exist in the preloaded
      dictionaries. New pronunciations are only added for words that are not
      present in the preloaded set.

  --replace
      Replace entries for words that already exist in the preloaded
      dictionaries. As soon as a word appears in the new dump, its existing
      pronunciations from the preloaded dictionaries are discarded and the
      new pronunciations become the reference set.

Input formats for <path-or-URL>:
  - Local files:
      - Plain XML dumps:  *.xml
      - Bzip2-compressed: *.xml.bz2, *.bz2
  - HTTP/HTTPS:
      When <path-or-URL> starts with "http://" or "https://", the dump is read
      directly from the HTTP response body as a stream. If the URL path ends
      with ".bz2" (e.g. Wikimedia dump URLs), the content is transparently
      decompressed on the fly without creating temporary files.

Examples:
  # Basic local scan (French, text export)
  ipadict parse --lang fr frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.txt

  # English Wiktionary dictionary
  ipadict parse --lang en enwiktionary-latest-pages-articles.xml.bz2 > exports/en.dict.txt

  # Explicit gob export
  ipadict parse --lang fr --export gob frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.gob

  # Merge existing French dictionaries with a new dump (append new pronunciations)
  ipadict parse --lang fr --preload exports/old.dict.txt --preload exports/custom.dict.txt --merge-append frwiktionary-new-pages-articles.xml.bz2 > exports/merged.dict.txt

  # Do not touch words that already exist in the preloaded dictionaries
  ipadict parse --lang fr --preload exports/curated.dict.txt --no-override frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.curated_plus_missing.dict.txt

  # Replace existing entries with new pronunciations when available
  ipadict parse --lang fr --preload exports/old.dict.txt --replace frwiktionary-20251120-pages-articles-multistream.xml.bz2 > exports/fr.override_old.dict.txt

  # Stream directly from Wikimedia dumps over HTTPS
  ipadict parse --lang fr https://dumps.wikimedia.org/frwiktionary/latest/frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.txt
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

// parseConfig holds options for the "parse" subcommand.
type parseConfig struct {
	Source       string              // path or URL
	ExportFormat string              // "text" or "gob"
	PreloadPaths []string            // optional, may be empty (can be specified multiple times)
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

// runParse executes a parse according to cfg and writes the result to stdout.
func runParse(cfg parseConfig) error {
	if cfg.Source == "" {
		return errors.New("missing <path-or-URL> argument")
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

	var rep *phonodict.Representation

	// Optionally preload one or more dictionaries (text, gob, ipa_dict_txt)
	// before scanning the dump.
	if len(cfg.PreloadPaths) > 0 {
		entries, seenWordPron, preloadedWords, err := phonodict.PreloadPaths(cfg.MergeMode, cfg.PreloadPaths...)
		if err != nil {
			return fmt.Errorf("preload %q: %w", strings.Join(cfg.PreloadPaths, ", "), err)
		}
		rep = &phonodict.Representation{
			Entries:        entries,
			SeenWordPron:   seenWordPron,
			PreloadedWords: preloadedWords,
		}
	} else {
		rep = phonodict.NewRepresentation()
	}

	parser := seqparser.NewXMLWikipediaDump(lang, cfg.MergeMode)
	parser.Progress = func(lines, words, uniquePairs int) {
		fmt.Fprintf(os.Stderr,
			"\rScanning... lines: %d (words: %d, unique word/pron pairs: %d)",
			lines, words, uniquePairs)
	}

	stats, err := parser.ParseSource(cfg.Source, rep)
	if err != nil {
		return fmt.Errorf("scan %q: %w", cfg.Source, err)
	}

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
		"\rFinished. Scanned lines: %d (words: %d, unique word/pron pairs: %d, elapsed: %.3f seconds)\n",
		stats.Lines, stats.Words, stats.UniquePairs, stats.Elapsed.Seconds())

	return nil
}

// runParseFromArgs parses flags/positional arguments for the "parse"
// subcommand and delegates to runParse.
func runParseFromArgs(args []string) error {
	fs := flag.NewFlagSet("parse", flag.ContinueOnError)

	exportFormat := fs.String("export", "text", "export format: text or gob")

	var preloadPaths stringSliceFlag
	fs.Var(&preloadPaths, "preload", "optional dictionary to preload (text, gob, ipa_dict_txt). Can be repeated.")

	lang := fs.String("lang", "fr", "language code to match in pron/API templates (e.g. fr, en, es, de)")

	mergeFlag := fs.Bool("merge", false, "alias for --merge-append (merge new pronunciations by appending them)")
	mergeAppendFlag := fs.Bool("merge-append", false, "merge new pronunciations into existing entries by appending them (default)")
	mergePrependFlag := fs.Bool("merge-prepend", false, "merge new pronunciations by prepending them before existing entries")

	noOverrideFlag := fs.Bool("no-override", false, "do not change entries for words that already exist in the preloaded dictionary")
	noOverrideCompat := fs.Bool("no-overide", false, "alias for --no-override")
	replaceFlag := fs.Bool("replace", false, "replace entries for words that already exist in the preloaded dictionary")

	fs.SetOutput(os.Stderr)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return nil
		}
		return err
	}

	remaining := fs.Args()
	if len(remaining) != 1 {
		return errors.New(`"parse" expects exactly one <path-or-URL> argument`)
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

	cfg := parseConfig{
		Source:       strings.TrimSpace(remaining[0]),
		ExportFormat: strings.TrimSpace(*exportFormat),
		PreloadPaths: preloadPaths,
		Lang:         strings.TrimSpace(*lang),
		MergeMode:    mode,
	}

	return runParse(cfg)
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return
	case "parse":
		if err := runParseFromArgs(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		log.Printf("Unknown subcommand %q\n\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(1)
	}
}
