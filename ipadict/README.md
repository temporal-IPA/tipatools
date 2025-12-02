# ipadict

`ipadict` is a small command‑line tool that builds IPA pronunciation dictionaries
from Wiktionary / Wikipedia XML dumps and various other sources.

It scans a dump (local file or HTTP/HTTPS URL, optionally bzip2‑compressed),
extracts IPA pronunciations from `{{pron|...}}` / `{{API|...}}` templates for a
given language code, and merges them with existing dictionaries. The resulting
dictionary can be exported as:

- a UTF‑8 text dictionary, or
- a gob‑encoded `map[string][]string` for fast re‑loading in Go programs.

The scanner is **streaming**: it never needs to load the full dump in memory.
This also applies to HTTP/HTTPS URLs (no temporary files).

---

## Installation

From the `tipatools/ipadict` directory:

```bash
go build -o bin/ipadict main.go
```

This will produce a `bin/ipadict` binary.

You can also install it in your `$GOBIN`:

```bash
go install ./...
```

(depending on how your module is laid out, you may want to run this from the
module root and adjust the path accordingly).

---

## Basic usage

The CLI no longer uses a `parse` subcommand. Instead you pass one or more
sources with `--parse` and let `ipadict` decide how to handle them.

General form:

```bash
ipadict [flags] --parse <path-or-URL> [--parse <path-or-URL> ...]
```

Each `--parse` source is processed in the order it appears on the command line.

- If it looks like a Wiktionary / Wikipedia XML dump (local file or HTTP/HTTPS
  URL, plain or `.bz2`‑compressed), it is scanned as a dump.
- Otherwise it is treated as an existing dictionary and loaded with the
  built‑in preloaders (native ipadict text, ipa‑dict “slashed” text, gob).

You can also preload dictionaries that should always come **before** everything
else using `--preload`.

```bash
ipadict [flags] --preload <dict> [--preload <dict> ...] --parse <src> [...]
```

### Examples

```bash
# French Wiktionary (local file, text export)
ipadict --lang fr        --parse frwiktionary-latest-pages-articles.xml.bz2        --export text        > exports/fr.dict.txt

# English Wiktionary (local file, text export)
ipadict --lang en        --parse enwiktionary-latest-pages-articles.xml.bz2        --export text        > exports/en.dict.txt

# French Wiktionary (HTTPS stream – no local file)
ipadict --lang fr        --parse https://dumps.wikimedia.org/frwiktionary/latest/frwiktionary-latest-pages-articles.xml.bz2        > exports/fr.dict.txt
```

---

## Input formats

`ipadict` accepts two kinds of sources via `--parse` (and the dictionary kind
via `--preload`):

### 1. XML dumps (Wiktionary / Wikipedia)

- **Local files**
    - Plain XML: `*.xml`
    - Bzip2‑compressed: `*.xml.bz2`, `*wiktionary*.bz2`, `*wikipedia*.bz2`
- **HTTP/HTTPS URLs**
    - Any URL starting with `http://` or `https://`
    - If the URL path ends with `.bz2`, the body is decompressed on the fly
      using `compress/bzip2`.

All dumps are scanned as streams; the tool only keeps a line buffer and the
resulting dictionary in memory.

### 2. Dictionary files

Dictionary files are handled by the `phonodict` preloaders and can be provided
either with `--parse` or `--preload`. Supported formats are automatically
detected (“sniffed”) from the first few kilobytes of the file and from the
file name:

- **Native ipadict text (`txt_tipa`)**

  ```text
  <word>	<IPA1> | <IPA2> | ...
  ```

  Example:

  ```text
  fauteuil	fo.tœj
  grand	gʁɑ̃ | gʁã
  ```

  Files with extensions such as `.txt` or `.txtipa` are treated as native text
  unless they clearly match another known format.

- **ipa‑dict style slashed text (`txt_slashed_tipa`)**

  ```text
  <word>	/<IPA>/
  <word>	/<IPA1>/ /<IPA2>/
  ```

  This is the format used by the `ipa-dict` project (`fr_FR.txt`, etc.).

- **Gob‑encoded dictionaries (`ipa_gob`)**

  Binary gob encoding of a `map[string][]string` produced by:

  ```bash
  ipadict --lang fr --export gob --parse dump.xml.bz2 > exports/fr.dict.gob
  ```

The sniffing logic has been tightened so that:

- Text dictionaries (`.txt`, `.txtipa`, ipa‑dict files) are always parsed with
  the appropriate text preloaders, even if they contain non‑ASCII or slightly
  unusual bytes.
- Gob files (`.gob`) are always handled by the gob preloader.
- Binary payloads that do not look like text are treated as gob only when
  there is no strong hint that they are text.

This makes **all supported types preloadable, sniffable and parsable** in a
predictable way.

---

## Output formats

Select with `--export`:

- `--export text` (default)

  Text dictionary on stdout, one entry per line:

  ```text
  fauteuil    fo.tœj
  grand       gʁɑ̃ | gʁã
  ```

  Each line is:

  ```text
  <word><TAB><IPA1> | <IPA2> | ...
  ```

- `--export gob`

  Gob‑encoded `map[string][]string` on stdout. This is useful when you want to
  reload the dictionary directly in Go:

  ```bash
  ipadict --lang fr --export gob          --parse frwiktionary-latest-pages-articles.xml.bz2          > exports/fr.dict.gob
  ```

  In Go:

  ```go
  f, _ := os.Open("exports/fr.dict.gob")
  defer f.Close()

  dec := gob.NewDecoder(f)
  var dict map[string][]string
  if err := dec.Decode(&dict); err != nil {
      log.Fatal(err)
  }
  ```

---

## Language selection (`--lang`)

Use `--lang` to select which language code to match in templates:

```bash
# French
ipadict --lang fr --parse ...

# English
ipadict --lang en --parse ...

# Spanish
ipadict --lang es --parse ...
```

The scanner looks for templates like:

- `{{pron|pʁɔ̃|fr}}`
- `{{pron|pɹəˈnaʊns|en}}`
- `{{API|…|es}}`

and only keeps parameters **before** the `<lang>` code that contain at least one
IPA character (as defined by the TIPA spec / `ipa.Charset`).

This makes `ipadict` usable for multiple languages as long as the dumps contain
standard `pron` / `API` templates with a language code.

---

## Preloading and merge modes

You can combine multiple sources — dumps and dictionaries — in a single run.
All of them are merged into a single internal representation using a selected
**merge mode**.

The order is:

1. All `--preload` dictionaries (in the order given).
2. All `--parse` sources (in the order given). Each `--parse` source may be
   either a dump or a dictionary.

Example:

```bash
ipadict --lang fr        --preload exports/user.dict.txt        --parse frwiktionary-latest-pages-articles.xml.bz2        --parse datasets/ipa-dict/fr_FR.txt        --merge-append        --export text        > exports/fr.merged.dict.txt
```

### `--preload PATH`

`PATH` is always treated as a dictionary and can be:

- a text dictionary produced by `ipadict`,
- a gob dictionary produced by `ipadict --export gob`, or
- an external text dictionary using the ipa‑dict encoding.

Preloaded dictionaries are merged first, in the order they are provided.

### `--parse PATH`

`PATH` is treated as:

- a **dump** when it looks like a Wiktionary / Wikipedia XML file or URL, or
- a **dictionary** otherwise, using the same supported dictionary formats as
  `--preload`.

This allows you to write linear pipelines such as:

```bash
ipadict --lang fr        --parse frwiktionary-20251120-pages-articles-multistream.xml        --parse datasets/ipa-dict/fr_FR.txt        --merge-append        --export text        > exports/fr.full.dict.txt
```

### Merge modes

Exactly one of the following may be specified (otherwise the default is used):

- `--merge-append` (or `--merge` – alias, for compatibility)

  New pronunciations are **appended after** the existing ones for each word.
  This is the default merge mode.

- `--merge-prepend`

  New pronunciations are **prepended before** existing ones for each word.

- `--no-override`

  Existing entries coming from preloaded sources are **never modified**. New
  pronunciations are only added for words that do **not** exist in the
  preloaded set.

- `--replace`

  When a word that already exists in the preloaded dictionaries appears in a
  new source, its preloaded pronunciations are **discarded**, and the new ones
  become the reference.

If no merge mode flag is given, `--merge-append` is used.

Internally, all merges (both preloads and parsed dumps / dictionaries) use the
same `phonodict.MergeMode` logic, so behaviour is consistent across sources.

---

## Progress reporting

When scanning very large dumps, `ipadict` prints a single‑line progress
indicator to **stderr** every N lines for each dump source:

```text
Scanning frwiktionary-20251120-pages-articles-multistream.xml... lines: 1200000 (words: 34567, unique word/pron pairs: 56789)
```

At the end of each dump it prints a per‑source summary, and at the very end it
prints a global summary across all sources:

```text
Finished frwiktionary-20251120-pages-articles-multistream.xml. Scanned lines: 12345678 (words: 89012, unique word/pron pairs: 123456, elapsed: 123.456 seconds)
Finished. Scanned lines: 12345678 (words: 89012, unique word/pron pairs: 123456, total elapsed: 123.456 seconds)
```

Because progress goes to stderr, you can safely redirect the dictionary on
stdout to a file:

```bash
ipadict --lang fr        --parse frwiktionary-20251120-pages-articles-multistream.xml.bz2        > exports/fr.dict.txt
```

and still see progress in the terminal.

Dictionary‑only sources (text / gob) are loaded without per‑line progress but
are accounted for in the final word / pair counts.

---

## Examples

### Build a French dictionary from a local dump

```bash
ipadict --lang fr        --parse frwiktionary-20251120-pages-articles-multistream.xml.bz2        --export text        > exports/fr.dict.txt
```

### Build an English dictionary from a remote dump

```bash
ipadict --lang en        --parse https://dumps.wikimedia.org/enwiktionary/latest/enwiktionary-latest-pages-articles.xml.bz2        --export text        > exports/en.dict.txt
```

### Merge a user dictionary (first) and a reference dump with append semantics

```bash
# user.dict.txt contains hand‑edited pronunciations
ipadict --lang fr        --preload exports/user.dict.txt        --parse frwiktionary-latest-pages-articles.xml.bz2        --merge-append        > exports/fr.user_first.dict.txt
```

### Merge a reference dictionary and then prepend user overrides

```bash
# preload a large reference dict, prepend user overrides from a new dump
ipadict --lang fr        --preload exports/reference.dict.txt        --parse frwiktionary-user-overrides.xml.bz2        --merge-prepend        > exports/fr.overrides_first.dict.txt
```

### Keep curated entries, only add missing words

```bash
ipadict --lang fr        --preload exports/curated.dict.txt        --parse frwiktionary-latest-pages-articles.xml.bz2        --no-override        > exports/fr.curated_plus_missing.dict.txt
```

### Let the new dump override an older dictionary

```bash
ipadict --lang fr        --preload exports/old.dict.txt        --parse frwiktionary-20251120-pages-articles-multistream.xml.bz2        --replace        > exports/fr.override_old.dict.txt
```

### One‑shot build like your shell script

Instead of two separate invocations:

```bash
ipadict parse --lang fr  "${WIKI_XML_PATH}" > "${TXT_IPA_PATH}"
ipadict parse --lang fr --preload "${TXT_IPA_PATH}" --merge-append "${IPA_DICT_FR}" > "${TXT_IPA_PATH}"
```

you can now do the equivalent in a single call:

```bash
ipadict --lang fr        --parse "${WIKI_XML_PATH}"        --parse "${IPA_DICT_FR}"        --merge-append        --export text        > "${TXT_IPA_PATH}"
```

The first `--parse` scans the Wiktionary dump, the second `--parse` loads the
ipa‑dict text file and appends pronunciations.

---

## Notes

- `ipadict` is language‑agnostic as long as the dumps contain `{{pron|...}}` /
  `{{API|...}}` templates with the language code as one of the parameters.
- IPA detection uses the TIPA character set (`ipa.Charset`), which includes
  IPA and ExtIPA symbols, diacritics and suprasegmentals.
- Only parameters that **look like IPA** (contain at least one IPA character)
  and appear **before** the language code are kept as pronunciations.
- The preloaders in `phonodict` are pluggable; external code can register
  additional textual formats while still benefiting from the common merge
  logic and de‑duplication.
