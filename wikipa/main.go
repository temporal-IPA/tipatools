// The command "wikipa" builds a pronunciation dictionary from a Wiktionary or Wikipedia dump.
//
// It scans an XML (uncompressed or .bz2, local or HTTP/HTTPS) and
// extracts IPA pronunciations from {{pron|...|<lang>}} and {{API|...|<lang>}}
// templates, where <lang> is a language code such as "fr", "en", "es".
// Results are exported either as a human-readable text dictionary
// or as a Go gob-encoded map[string][]string for fast reloading.
//
// Example usages:
//
//   # Explicit text export (French):
//   wikipa parse --lang fr --export text frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.txt
//
//   # English dictionary example (Wiktionary):
//   wikipa parse --lang en --export text enwiktionary-latest-pages-articles.xml.bz2 > exports/en.dict.txt
//
//   # Gob export (binary map[string][]string):
//   wikipa parse --lang fr --export gob frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.gob
//
//   # Merge with a pre-existing dictionary (text or gob):
//   wikipa parse --lang fr --preload fr.dict.txt --merge-append frwiktionary-new-pages-articles.xml.bz2 > exports/merged.dict.txt
//
//   # Stream directly from Wikimedia dumps over HTTPS (no local file):
//   wikipa parse --lang fr https://dumps.wikimedia.org/frwiktionary/latest/frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.txt
//
// The scanner operates in a streaming fashion: it never needs to load the full
// dump into memory. When given an HTTP(S) URL, the tool reads from the response
// body as it arrives, and transparently decompresses .bz2 payloads on the fly.

package main

import (
	"bufio"
	"compress/bzip2"
	_ "embed"
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/temporal-IPA/tipa/pkg/ipa"
	"golang.org/x/net/html"
)

// --- Regexes used by the scanner --------------------------------------------

// headwordRegex extracts the headword from lines like:
//
//	'''fauteuil''' {{pron|fo.tœj|fr}} {{m}}
var headwordRegex = regexp.MustCompile(`'''([^']+)'''`)

// pronTemplateRegex extracts full pron/API templates like:
//
//	{{pron|pʁɔ̃|fr}}, {{pron|pʁɔ̃|pʁã|fr}}, {{API|…|fr}}
var pronTemplateRegex = regexp.MustCompile(`\{\{(?:pron|API)\|([^}]*)\}\}`)

// htmlTagRegexp strips HTML-ish tags like <small>…</small>, <sup>6</sup>, <span...>, etc.
var htmlTagRegexp = regexp.MustCompile(`<[^>]+>`)

// interwikiPrefixRegex strips prefixes like :fr:foo, :en:bar, :it:JeanJean.
var interwikiPrefixRegex = regexp.MustCompile(`^:([a-z]{2,3}):(.+)$`)

// --- CLI help / usage -------------------------------------------------------

const helpText = `wikipa - Wiktionary / Wikipedia IPA pronunciation scanner

Usage:
  wikipa help
      Print this help message.

  wikipa parse [flags] <path-or-URL>
      Parse a local dump file or an HTTP/HTTPS URL and emit a
      pronunciation dictionary.

Flags for "parse":
  --lang CODE
      Language code to match in {{pron|...}} / {{API|...}} templates.
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
          wikipa parse --export gob dump.xml.bz2 > fr.dict.gob

  --preload PATH
      Preload an existing dictionary before scanning <path-or-URL>.
      PATH can be either:
        - a text dictionary produced by this tool (format above), or
        - a gob file produced by "wikipa parse --export gob".
      Entries from PATH are combined with the newly scanned dump using one of
      the merge modes below.

  --merge-append
      Merge new pronunciations into the existing dictionary by appending them
      after existing entries (default). New pronunciations for a word are added
      at the end of the existing list, with de-duplication on (word, pronunciation).

  --merge-prepend
      Merge new pronunciations by prepending them before existing entries for
      each word. This is useful when the newly parsed dump should have higher
      priority than the preloaded dictionary.

  --merge
      Alias for --merge-append (kept for backward compatibility).

  --no-override
      Do not change entries for words that already exist in the preloaded
      dictionary. New pronunciations are only added for words that are not
      present in the preloaded dictionary.

  --replace
      Replace entries for words that already exist in the preloaded
      dictionary. As soon as a word appears in the new dump, its existing
      pronunciations from the preloaded dictionary are discarded and the
      new pronunciations become the reference set.

Input formats:
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
  wikipa parse --lang fr frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.txt

  # English Wiktionary dictionary
  wikipa parse --lang en enwiktionary-latest-pages-articles.xml.bz2 > exports/en.dict.txt

  # Explicit gob export
  wikipa parse --lang fr --export gob frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.gob

  # Merge an existing French dictionary with a new dump (append new pronunciations)
  wikipa parse --lang fr --preload fr.dict.txt --merge-append frwiktionary-new-pages-articles.xml.bz2 > exports/merged.dict.txt

  # Preload a reference dictionary, then prepend user overrides from a new dump
  wikipa parse --lang fr --preload reference.dict.txt --merge-prepend user-overrides.xml.bz2 > exports/fr.overrides_first.dict.txt

  # Do not touch words that already exist in the preloaded dictionary
  wikipa parse --lang fr --preload fr.dict.txt --no-override frwiktionary-new-pages-articles.xml.bz2 > exports/fr.dict.txt

  # Replace existing entries with new pronunciations when available
  wikipa parse --lang fr --preload fr.base.dict.txt --replace frwiktionary-new-pages-articles.xml.bz2 > exports/fr.dict.txt

  # Stream directly from Wikimedia dumps over HTTPS
  wikipa parse --lang fr https://dumps.wikimedia.org/frwiktionary/latest/frwiktionary-latest-pages-articles.xml.bz2 > exports/fr.dict.txt
`

// printUsage writes the CLI help text to the given writer.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, helpText)
}

// --- File / URL open helpers ------------------------------------------------

// openLocalPossiblyCompressed opens a local file and wraps it in a bzip2
// decompressor when the path ends with ".bz2". The returned ReadCloser always
// closes the underlying file.
func openLocalPossiblyCompressed(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".bz2") {
		// Wrap the bzip2 reader to preserve the Close method on the file.
		return struct {
			io.Reader
			io.Closer
		}{
			Reader: bzip2.NewReader(f),
			Closer: f,
		}, nil
	}

	return f, nil
}

// isHTTPURL returns true if src looks like an HTTP or HTTPS URL.
func isHTTPURL(src string) bool {
	return strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")
}

// hasBZ2SuffixURL reports whether a URL string should be treated as a .bz2
// resource, ignoring query or fragment parts.
func hasBZ2SuffixURL(raw string) bool {
	lower := strings.ToLower(raw)
	if idx := strings.IndexAny(lower, "?#"); idx >= 0 {
		lower = lower[:idx]
	}
	return strings.HasSuffix(lower, ".bz2")
}

// openHTTPPossiblyCompressed performs an HTTP GET and returns a streaming
// reader, wrapping the response body in a bzip2 decompressor when the URL
// indicates a .bz2 payload.
//
// No temporary files are created: the caller reads directly from the HTTP
// response stream.
func openHTTPPossiblyCompressed(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url) // #nosec G107 - URL is user-provided by design.
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("HTTP GET %s: unexpected status %s", url, resp.Status)
	}

	// For Wikimedia dumps and similar, the URL path usually ends with ".bz2".
	if hasBZ2SuffixURL(url) {
		return struct {
			io.Reader
			io.Closer
		}{
			Reader: bzip2.NewReader(resp.Body),
			Closer: resp.Body,
		}, nil
	}

	return resp.Body, nil
}

// openSource opens either a local file or an HTTP/HTTPS URL and wraps it in a
// bzip2 decompressor when appropriate. The returned ReadCloser must be closed
// by the caller.
func openSource(pathOrURL string) (io.ReadCloser, error) {
	if isHTTPURL(pathOrURL) {
		return openHTTPPossiblyCompressed(pathOrURL)
	}
	return openLocalPossiblyCompressed(pathOrURL)
}

// --- Extraction helpers -----------------------------------------------------

// extractPronunciationsFromLine extracts IPA pronunciations from a single line
// containing one or more {{pron|...|<lang>}} or {{API|...|<lang>}} templates.
//
// It performs a fast local de-duplication per line to reduce downstream work,
// and only keeps parameters that both:
//   - appear before the language marker, and
//   - contain at least one character from ipa.Charset.
func extractPronunciationsFromLine(line string, lang string) []string {
	// Extract all pron/API templates from the line in one pass.
	matches := pronTemplateRegex.FindAllStringSubmatch(line, -1)
	if len(matches) == 0 {
		return nil
	}

	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		lang = "fr"
	}

	// Local dedup for this line only, to avoid extra work downstream.
	seen := make(map[string]struct{})
	var out []string

	for _, m := range matches {
		raw := m[1]
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, "|")

		// Must include the target language somewhere in the template parameters.
		isTargetLang := false
		for _, p := range parts {
			if strings.ToLower(strings.TrimSpace(p)) == lang {
				isTargetLang = true
				break
			}
		}
		if !isTargetLang {
			continue
		}

		// Collect IPA values until the language marker is hit.
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if strings.ToLower(p) == lang {
				break
			}
			if p == "" {
				continue
			}
			// Fast filter: keep only parameters that look like IPA.
			if !strings.ContainsAny(p, ipa.Charset) {
				continue
			}

			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				out = append(out, p)
			}
		}
	}

	return out
}

// The replace / replacer pair is used to normalize a headword by removing
// various wiki markup characters that should not appear in dictionary keys.
var replace = []string{
	" ", "",
	"\t", "",
	"[", "",
	"]", "",
	"{", "",
	"}", "",
	"+", "",
	"(", "",
	")", "",
	"|", "",
}

var replacer = strings.NewReplacer(replace...)

// normalizeHeadword cleans and filters a raw headword string.
//
// It:
//   - decodes HTML entities,
//   - strips HTML-ish tags (<small>, <sup>, malformed <spanstyle=...>, ...),
//   - strips interwiki prefixes like :fr:foo, :en:bar,
//   - applies the replacer (removing spaces, [], {}, +, (), |),
//   - trims simple trailing punctuation,
//   - rejects lines that look like bullets (#...) or contain no letters,
//   - rejects tiny slash-based artifacts like "s/s".
func normalizeHeadword(raw string) string {
	if raw == "" {
		return ""
	}

	// Decode HTML entities (&#43; -> +, &eacute; -> é, etc.).
	raw = html.UnescapeString(raw)

	// Strip HTML-ish tags.
	raw = htmlTagRegexp.ReplaceAllString(raw, "")
	raw = strings.TrimSpace(raw)

	// Strip interwiki prefixes like :fr:atchourissage -> atchourissage.
	if m := interwikiPrefixRegex.FindStringSubmatch(raw); len(m) == 3 {
		raw = strings.TrimSpace(m[2])
	}

	// Apply basic replacer (removes spaces, [], {}, +, (), |).
	raw = replacer.Replace(raw)
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return ""
	}

	// Drop wiki list / heading artifacts like "#Prononciationdumotmoelleux."
	if strings.HasPrefix(raw, "#") {
		return ""
	}

	// Trim simple trailing punctuation often coming from titles.
	raw = strings.Trim(raw, ".,;:")

	if raw == "" {
		return ""
	}

	// Require at least one letter.
	hasLetter := false
	letterCount := 0
	for _, r := range raw {
		if unicode.IsLetter(r) {
			hasLetter = true
			letterCount++
		}
	}
	if !hasLetter {
		return ""
	}

	// Drop tiny slash-based artifacts (e.g. "s/s") while keeping more substantial
	// things like "et/ou" if they ever appear.
	if strings.Contains(raw, "/") && letterCount <= 2 {
		return ""
	}

	return raw
}

// extractHeadwordFromLine returns a normalized headword for the current line,
// falling back to the page title when no explicit ”'headword”' is present.
//
// Examples of possible inputs:
//
//	<title>fauteuil</title>
//	'''fauteuil''' {{pron|fo.tœj|fr}} {{m}}
func extractHeadwordFromLine(line, title string) string {
	raw := title
	if m := headwordRegex.FindStringSubmatch(line); len(m) > 1 {
		raw = strings.TrimSpace(m[1])
	}
	return normalizeHeadword(raw)
}

// --- Dictionary preload / export helpers ------------------------------------

// mergeMode controls how a preloaded dictionary and a newly scanned dump
// are combined when the same headword appears in both.
type mergeMode int

const (
	mergeModeAppend mergeMode = iota
	mergeModePrepend
	mergeModeNoOverride
	mergeModeReplace
)

// preloadDictionary loads an existing dictionary from PATH and merges it into
// entries / seenWordPron.
//
// Supported formats:
//   - Gob (encoding/gob) containing a map[string][]string.
//   - Text dictionary produced by this tool (one "<word>\t<IPA1> | <IPA2>..." per line).
//
// preloadedWords is populated with all words that originate from PATH.
// This information is later used to decide how to handle the merge modes.
func preloadDictionary(path string, entries map[string][]string, seenWordPron map[string]struct{}, preloadedWords map[string]struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Try gob first.
	if err := func() error {
		dec := gob.NewDecoder(f)
		var dict map[string][]string
		if err := dec.Decode(&dict); err != nil {
			return err
		}
		// Merge into the live dictionary with global de-duplication.
		for w, prons := range dict {
			preloadedWords[w] = struct{}{}
			baseKey := w + "\x00"
			for _, p := range prons {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				key := baseKey + p
				if _, ok := seenWordPron[key]; ok {
					continue
				}
				seenWordPron[key] = struct{}{}
				entries[w] = append(entries[w], p)
			}
		}
		return nil
	}(); err == nil {
		// Gob decoding succeeded; no need to try text.
		return nil
	}

	// Rewind and try text dictionary format.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		word := strings.TrimSpace(parts[0])
		rawProns := strings.TrimSpace(parts[1])
		if word == "" || rawProns == "" {
			continue
		}
		preloadedWords[word] = struct{}{}
		baseKey := word + "\x00"

		// The text format uses " | " as separator between multiple pronunciations.
		chunks := strings.Split(rawProns, "|")
		for _, c := range chunks {
			p := strings.TrimSpace(c)
			if p == "" {
				continue
			}
			key := baseKey + p
			if _, ok := seenWordPron[key]; ok {
				continue
			}
			seenWordPron[key] = struct{}{}
			entries[word] = append(entries[word], p)
		}
	}
	return scanner.Err()
}

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

// --- Core scanner -----------------------------------------------------------

// scanDump reads a dump from reader, updating entries and seenWordPron in place.
//
// preloadedWords contains all words that came from a preloaded dictionary
// (if any) and is used to implement the merge modes.
//
// It returns:
//   - lineCount: number of lines scanned from the dump,
//   - wordCount: number of unique words in the resulting dictionary.
func scanDump(
	reader io.Reader,
	entries map[string][]string,
	seenWordPron map[string]struct{},
	preloadedWords map[string]struct{},
	mode mergeMode,
	lang string,
) (lineCount int, wordCount int, err error) {
	scanner := bufio.NewScanner(reader)

	// Larger initial buffer for long Wiktionary lines.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	var (
		title  string
		inText bool
	)

	const progressStep = 100000

	// For --replace, we only want to discard preloaded entries for a word once.
	replaced := make(map[string]struct{})

	for scanner.Scan() {
		line := scanner.Text()
		lineCount++

		// Periodic single-line progress on stderr.
		if lineCount%progressStep == 0 {
			fmt.Fprintf(os.Stderr,
				"\rScanning... lines: %d (words: %d, unique word/pron pairs: %d)",
				lineCount, len(entries), len(seenWordPron))
		}

		// Detect page title on lines containing both <title> and </title>.
		if strings.Contains(line, "<title>") && strings.Contains(line, "</title>") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "<title>") && strings.Contains(trim, "</title>") {
				start := strings.Index(trim, "<title>") + len("<title>")
				end := strings.Index(trim, "</title>")
				if end > start {
					title = trim[start:end]
				}
			}
		}

		// Detect entering/leaving text node.
		if strings.Contains(line, "<text") {
			inText = true
		}
		if strings.Contains(line, "</text>") {
			inText = false
		}

		// Only parse inside text nodes.
		if !inText {
			continue
		}

		// Quick reject: skip lines without pron/API templates.
		if !strings.Contains(line, "{{pron|") && !strings.Contains(line, "{{API|") {
			continue
		}

		word := extractHeadwordFromLine(line, title)
		if word == "" {
			continue
		}

		// In --no-override mode, words that already exist in the preloaded
		// dictionary are left untouched: ignore all new pronunciations.
		if mode == mergeModeNoOverride {
			if _, pre := preloadedWords[word]; pre {
				continue
			}
		}

		prons := extractPronunciationsFromLine(line, lang)
		if len(prons) == 0 {
			continue
		}

		// In --replace mode, the first time we see a word that comes from
		// the preloaded dictionary, we discard its existing pronunciations
		// and start a fresh set from the new dump.
		if mode == mergeModeReplace {
			if _, pre := preloadedWords[word]; pre {
				if _, already := replaced[word]; !already {
					for _, old := range entries[word] {
						delete(seenWordPron, word+"\x00"+old)
					}
					entries[word] = nil
					replaced[word] = struct{}{}
				}
			}
		}

		// Aggregate pronunciations per word with global dedup on (word, pron).
		baseKey := word + "\x00"
		for _, p := range prons {
			key := baseKey + p
			if _, ok := seenWordPron[key]; ok {
				continue
			}
			seenWordPron[key] = struct{}{}

			switch mode {
			case mergeModePrepend:
				// New pronunciations come first.
				entries[word] = append([]string{p}, entries[word]...)
			default:
				// Append mode (including no-override & replace).
				entries[word] = append(entries[word], p)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return lineCount, len(entries), err
	}

	return lineCount, len(entries), nil
}

// --- CLI wiring -------------------------------------------------------------

// parseConfig holds options for the "parse" subcommand.
type parseConfig struct {
	Source       string    // path or URL
	ExportFormat string    // "text" or "gob"
	PreloadPath  string    // optional, may be empty
	Lang         string    // language code used in pron/API templates
	MergeMode    mergeMode // append, prepend, no-override, replace
}

// runParse executes a parse according to cfg and writes the result to stdout.
func runParse(cfg parseConfig) error {
	if cfg.Source == "" {
		return errors.New("missing <path-or-URL> argument")
	}

	export := strings.ToLower(cfg.ExportFormat)
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

	entries := make(map[string][]string, 1<<16)
	seenWordPron := make(map[string]struct{}, 1<<18)
	preloadedWords := make(map[string]struct{})

	// Optionally preload an existing dictionary (text or gob) before scanning.
	if cfg.PreloadPath != "" {
		if err := preloadDictionary(cfg.PreloadPath, entries, seenWordPron, preloadedWords); err != nil {
			return fmt.Errorf("preload %q: %w", cfg.PreloadPath, err)
		}
	}

	ts := time.Now()

	reader, err := openSource(cfg.Source)
	if err != nil {
		return fmt.Errorf("open %q: %w", cfg.Source, err)
	}
	defer reader.Close()

	lineCount, wordCount, err := scanDump(reader, entries, seenWordPron, preloadedWords, cfg.MergeMode, lang)
	if err != nil {
		return fmt.Errorf("scan %q: %w", cfg.Source, err)
	}

	switch export {
	case "text":
		if err := writeTextDictionary(os.Stdout, entries); err != nil {
			return fmt.Errorf("write text: %w", err)
		}
	case "gob":
		if err := writeGobDictionary(os.Stdout, entries); err != nil {
			return fmt.Errorf("write gob: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr,
		"\rFinished. Scanned lines: %d (words: %d, unique word/pron pairs: %d, elapsed: %.3f seconds)\n",
		lineCount, wordCount, len(seenWordPron), time.Since(ts).Seconds())

	return nil
}

// runParseFromArgs parses flags/positional arguments for the "parse"
// subcommand and delegates to runParse.
func runParseFromArgs(args []string) error {
	fs := flag.NewFlagSet("parse", flag.ContinueOnError)

	exportFormat := fs.String("export", "text", "export format: text or gob")
	preloadPath := fs.String("preload", "", "optional dictionary to preload (text or gob)")
	lang := fs.String("lang", "fr", "language code to match in pron/API templates (e.g. fr, en, es, de)")

	mergeFlag := fs.Bool("merge", false, "alias for --merge-append (merge new pronunciations by appending them)")
	mergeAppendFlag := fs.Bool("merge-append", false, "merge new pronunciations into existing entries by appending them (default)")
	mergePrependFlag := fs.Bool("merge-prepend", false, "merge new pronunciations by prepending them before existing entries")

	noOverrideFlag := fs.Bool("no-override", false, "do not change entries for words that already exist in the preloaded dictionary")
	// Optional compatibility flag for the misspelled variant.
	noOverrideCompat := fs.Bool("no-overide", false, "alias for --no-override")
	replaceFlag := fs.Bool("replace", false, "replace entries for words that already exist in the preloaded dictionary")

	// Direct flag.Parse output to stderr for clarity.
	fs.SetOutput(os.Stderr)

	if err := fs.Parse(args); err != nil {
		// If the user asked for help just print the global help.
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

	// Determine merge mode; default to append.
	mode := mergeModeAppend
	selected := 0

	if *mergeFlag || *mergeAppendFlag {
		mode = mergeModeAppend
		selected++
	}
	if *mergePrependFlag {
		mode = mergeModePrepend
		selected++
	}
	if *noOverrideFlag || *noOverrideCompat {
		mode = mergeModeNoOverride
		selected++
	}
	if *replaceFlag {
		mode = mergeModeReplace
		selected++
	}

	if selected > 1 {
		return errors.New("only one of --merge/--merge-append, --merge-prepend, --no-override/--no-overide, or --replace may be specified")
	}

	cfg := parseConfig{
		Source:       strings.TrimSpace(remaining[0]),
		ExportFormat: strings.TrimSpace(*exportFormat),
		PreloadPath:  strings.TrimSpace(*preloadPath),
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
