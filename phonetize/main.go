package main

// phonetize --load-dict <dict path> --load-final-dict <dict path> --file <file path to tokenize> or --sentence  "the sentence"
//
// This tool is a small wrapper around the g2p.Determinist scanner.
// It loads one mandatory main dictionary and one optional "final"
// fallback dictionary, then runs the scanner on either a sentence
// provided on the command line or the contents of a text file.
//
// Example usage:
//
//   phonetize \
//     --load-dict path/to/main.dict \
//     --load-final-dict path/to/fallback.dict \
//     --sentence "Bonjour les amis." \
//     --output txt
//
// The --output flag controls what is printed:
//
//   - --output json
//       Prints the full g2p.Result as JSON (fragments + raw_texts).
//
//   - --output txt
//       Rebuilds a linear textual representation by concatenating, in
//       order, all fragments and raw texts:
//         * each Fragment contributes its IPA transcription
//         * each RawText contributes its original surface Text
//
//       This effectively produces an "IPA string with holes": anything
//       the dictionaries could phonetize is printed as IPA; everything
//       else is preserved verbatim.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/temporal-IPA/tipa/pkg/g2p"
	"github.com/temporal-IPA/tipa/pkg/phono"
)

// command line flags
var (
	flagDictPath      = flag.String("load-dict", "", "path to the main phonetic dictionary (required)")
	flagFinalDictPath = flag.String("load-final-dict", "", "optional path to the fallback phonetic dictionary")
	flagFilePath      = flag.String("file", "", "path to a text file to phonetize")
	flagSentence      = flag.String("sentence", "", "sentence to phonetize (mutually exclusive with --file)")
	flagOutput        = flag.String("output", "json", "output format: json or txt")
)

// main is the entry point of the phonetize CLI.
//
// It parses command line flags, loads the dictionaries using
// phono.LoadPaths with MergeModeAppend (one independent call per
// dictionary), builds a g2p.Determinist instance and runs a scan
// over the requested input text.
func main() {
	configureUsage()
	flag.Parse()

	// Validate CLI arguments.
	if strings.TrimSpace(*flagDictPath) == "" {
		failf("missing required flag: --load-dict <dict path>")
	}

	hasFile := strings.TrimSpace(*flagFilePath) != ""
	hasSentence := strings.TrimSpace(*flagSentence) != ""

	if hasFile == hasSentence {
		// Either both are set, or neither.
		failf("you must specify exactly one of --file or --sentence")
	}

	outputMode := strings.ToLower(strings.TrimSpace(*flagOutput))
	if outputMode == "" {
		outputMode = "json"
	}
	if outputMode != "json" && outputMode != "txt" {
		failf("invalid --output value %q (expected \"json\" or \"txt\")", *flagOutput)
	}

	// Load the main dictionary (required).
	mainDict, err := loadDictionaryFromPath(*flagDictPath)
	if err != nil {
		failf("failed to load main dictionary from %q: %v", *flagDictPath, err)
	}

	// Load the optional final dictionary (may be nil).
	var finalDict phono.Dictionary
	if strings.TrimSpace(*flagFinalDictPath) != "" {
		finalDict, err = loadDictionaryFromPath(*flagFinalDictPath)
		if err != nil {
			failf("failed to load final dictionary from %q: %v", *flagFinalDictPath, err)
		}
	}

	// Build the Determinist g2p processor.
	//
	// The scanner is run in tolerant mode so that diacritics may be
	// ignored when helpful (e.g. "garcon" vs "garçon").
	d := g2p.NewDeterminist(mainDict, finalDict)

	inputText, err := readInputText(hasFile, *flagFilePath, *flagSentence)
	if err != nil {
		failf("%v", err)
	}

	result := d.Scan(inputText, true)

	switch outputMode {
	case "json":
		if err := printJSONResult(result); err != nil {
			failf("failed to encode result as JSON: %v", err)
		}
	case "txt":
		text := composeText(result)
		fmt.Println(text)
	default:
		// Should never happen thanks to earlier validation.
		failf("unsupported output mode %q", outputMode)
	}
}

// configureUsage installs a custom usage message that documents the
// expected flags and overall behaviour of the CLI.
func configureUsage() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintln(out, "Usage:")
		fmt.Fprintln(out, "  phonetize --load-dict <dict path> [--load-final-dict <dict path>] (--file <file path> | --sentence \"text\") [--output json|txt]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Flags:")
		flag.PrintDefaults()
	}
}

// loadDictionaryFromPath loads a single dictionary file using
// phono.LoadPaths with MergeModeAppend.
//
// Each dictionary (main and final) is loaded independently: they are
// not merged together. The path may be absolute or relative.
func loadDictionaryFromPath(path string) (phono.Dictionary, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("empty dictionary path")
	}

	// Normalize the path and split it into (directory, file).
	//
	// We then create an fs.FS rooted at the directory, and pass only
	// the file name to LoadPaths. This works for both absolute and
	// relative paths.
	clean := filepath.Clean(path)
	dir, file := filepath.Split(clean)
	if file == "" {
		return nil, fmt.Errorf("dictionary path %q has no file component", path)
	}
	if dir == "" {
		dir = "."
	}

	fsys := os.DirFS(dir)
	dict, err := phono.LoadPaths(fsys, phono.MergeModeAppend, file)
	if err != nil {
		return nil, err
	}

	return dict, nil
}

// readInputText returns the text to phonetize, coming either from a
// file (--file) or directly from the command line (--sentence).
//
// Exactly one of hasFile / sentence must be set; this is enforced in
// main() before calling this helper.
func readInputText(hasFile bool, filePath, sentence string) (string, error) {
	if hasFile {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to read input file %q: %w", filePath, err)
		}
		return string(content), nil
	}
	// Direct sentence input.
	return sentence, nil
}

// printJSONResult marshals the g2p.Result into indented JSON and
// writes it to standard output.
func printJSONResult(res g2p.Result) error {
	encoded, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(encoded)
	if err != nil {
		return err
	}
	// Trailing newline for nicer shells.
	_, err = os.Stdout.Write([]byte("\n"))
	return err
}

// composeText rebuilds a linear textual representation from a g2p.Result.
//
// The Determinist scanner guarantees that Fragments and RawTexts are
// positioned in rune offsets relative to the original input text and
// that they cover it without overlap: each rune belongs either to a
// Fragment or to a RawText, but never both.
//
// For the textual output mode we simply:
//
//   - sort all segments (fragments + raw_texts) by Pos
//   - concatenate their textual representation:
//   - Fragment -> its IPA transcription
//   - RawText  -> its original Text
//
// This yields a single string where known pieces of text are replaced
// by their IPA form, while unknown spans, spaces and punctuation are
// preserved as-is.
func composeText(res g2p.Result) string {
	type segment struct {
		pos  int
		text string
	}

	segs := make([]segment, 0, len(res.Fragments)+len(res.RawTexts))

	for _, f := range res.Fragments {
		segs = append(segs, segment{
			pos:  f.Pos,
			text: string(f.IPA),
		})
	}
	for _, rt := range res.RawTexts {
		segs = append(segs, segment{
			pos:  rt.Pos,
			text: rt.Text,
		})
	}

	// Sort segments by their starting position.
	// When positions are equal (which should not normally happen for
	// non-overlapping segments), keep the original order.
	if len(segs) > 1 {
		// Simple insertion sort is enough here; the total number of
		// segments for a sentence is usually very small.
		for i := 1; i < len(segs); i++ {
			j := i
			for j > 0 && segs[j-1].pos > segs[j].pos {
				segs[j-1], segs[j] = segs[j], segs[j-1]
				j--
			}
		}
	}

	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.text)
	}
	return b.String()
}

// failf prints a formatted error message to standard error and exits
// the process with a non-zero status code.
func failf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "phonetize:", msg)
	os.Exit(1)
}
