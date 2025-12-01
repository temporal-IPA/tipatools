# wikipa

`wikipa` is a small command‑line tool that builds IPA pronunciation dictionaries
from Wiktionary / Wikipedia XML dumps.

It scans a dump (local file or HTTP/HTTPS URL, optionally bzip2‑compressed),
extracts IPA pronunciations from `{{pron|...}}` / `{{API|...}}` templates for a
given language code, and exports the result as:

- a UTF‑8 text dictionary, or
- a gob‑encoded `map[string][]string` for fast re‑loading in Go programs.

The scanner is **streaming**: it never needs to load the full dump in memory.
This also applies to HTTP/HTTPS URLs (no temporary files).

---

## Installation

From the `tipatools/wikipa` directory:

```bash
go build -o bin/wikipa main.go
```

This will produce a `bin/wikipa` binary.

You can also install it in your `$GOBIN`:

```bash
go install ./...
```

(depending on how your module is laid out, you may want to run this from the
module root and adjust the path accordingly).

---

## Basic usage

General form:

```bash
wikipa parse [flags] <path-or-URL>
```

Examples:

```bash
# French Wiktionary (local file, text export)
wikipa parse --lang fr   --export text   frwiktionary-latest-pages-articles.xml.bz2   > exports/fr.dict.txt

# English Wiktionary (local file, text export)
wikipa parse --lang en   --export text   enwiktionary-latest-pages-articles.xml.bz2   > exports/en.dict.txt

# French Wiktionary (HTTPS stream – no local file)
wikipa parse --lang fr   https://dumps.wikimedia.org/frwiktionary/latest/frwiktionary-latest-pages-articles.xml.bz2   > exports/fr.dict.txt
```

---

## Input formats

`wikipa` accepts:

- **Local files**
    - Plain XML: `*.xml`
    - Bzip2‑compressed: `*.xml.bz2`, `*.bz2`
- **HTTP/HTTPS URLs**
    - Any URL starting with `http://` or `https://`
    - If the URL path ends with `.bz2`, the body is decompressed on the fly
      using `compress/bzip2`.

All inputs are scanned as streams; the tool only keeps a line buffer and the
resulting dictionary in memory.

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

  ```
  <word><TAB><IPA1> | <IPA2> | ...
  ```

- `--export gob`

  Gob‑encoded `map[string][]string` on stdout. This is useful when you want to
  reload the dictionary directly in Go:

  ```bash
  wikipa parse --lang fr --export gob dump.xml.bz2 > exports/fr.dict.gob
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
wikipa parse --lang fr ...

# English
wikipa parse --lang en ...

# Spanish
wikipa parse --lang es ...
```

The scanner looks for templates like:

- `{{pron|pʁɔ̃|fr}}`
- `{{pron|pɹəˈnaʊns|en}}`
- `{{API|…|es}}`

and only keeps parameters **before** the `<lang>` code that contain at least one
IPA character (as defined by the TIPA spec / `ipa.Charset`).

This makes `wikipa` usable for multiple languages as long as the dumps contain
standard `pron` / `API` templates with a language code.

---

## Preloading and merge modes

You can preload an existing dictionary and then merge the new dump into it.

```bash
wikipa parse --lang fr   --preload exports/user.dict.txt   --merge-append   frwiktionary-latest-pages-articles.xml.bz2   > exports/fr.merged.dict.txt
```

### `--preload PATH`

`PATH` can be:

- a text dictionary produced by `wikipa`, or
- a gob dictionary produced by `wikipa parse --export gob`.

The preloaded dictionary is combined with the newly scanned dump according to a
**merge mode**.

### Merge modes

Exactly one of the following may be specified (otherwise the default is used):

- `--merge-append` (or `--merge` – alias, for compatibility)

  New pronunciations are **appended after** the existing ones for each word.
  This is the default merge mode.

  Example: preloaded dictionary is considered the “user” preferences, and the
  new dump is a larger reference dictionary. User picks come first if the user
  dictionary is preloaded and the reference dump is parsed afterwards.

- `--merge-prepend`

  New pronunciations are **prepended before** existing ones for each word.

  Example: preload a reference dictionary, then parse a dump containing user
  overrides; user pronunciations will appear first.

- `--no-override`

  Existing entries coming from `--preload` are **never modified**. New
  pronunciations are only added for words that do **not** exist in the
  preloaded dictionary.

  This is useful when you want to “fill in the gaps” of a curated dictionary
  with extra entries from a larger dump, without touching the curated ones.

- `--replace`

  When a word that already exists in the preloaded dictionary appears in the
  new dump, its preloaded pronunciations are **discarded**, and the new ones
  become the reference.

  This is useful when you want the dump to override an older dictionary.

If no merge mode flag is given, `--merge-append` is used.

---

## Progress reporting

When scanning very large dumps, `wikipa` prints a single‑line progress indicator
to **stderr** every N lines:

```text
Scanning... lines: 1200000 (words: 34567, unique word/pron pairs: 56789)
```

At the end, it prints a final summary:

```text
Finished. Scanned lines: 12345678 (words: 89012, unique word/pron pairs: 123456, elapsed: 123.456 seconds)
```

Because progress goes to stderr, you can safely redirect the dictionary on
stdout to a file:

```bash
wikipa parse --lang fr dump.xml.bz2 > exports/fr.dict.txt
```

and still see progress in the terminal.

---

## Examples

### Build a French dictionary from a local dump

```bash
wikipa parse --lang fr   --export text   frwiktionary-20251120-pages-articles-multistream.xml.bz2   > exports/fr.dict.txt
```

### Build an English dictionary from a remote dump

```bash
wikipa parse --lang en   https://dumps.wikimedia.org/enwiktionary/latest/enwiktionary-latest-pages-articles.xml.bz2   > exports/en.dict.txt
```

### Merge a user dictionary (first) and a reference dump with append semantics

```bash
# user.dict.txt contains hand‑edited pronunciations
wikipa parse --lang fr   --preload exports/user.dict.txt   --merge-append   frwiktionary-latest-pages-articles.xml.bz2   > exports/fr.user_first.dict.txt
```

### Merge a reference dictionary and then prepend user overrides

```bash
# preload a large reference dict, prepend user overrides from a new dump
wikipa parse --lang fr   --preload exports/reference.dict.txt   --merge-prepend   frwiktionary-user-overrides.xml.bz2   > exports/fr.overrides_first.dict.txt
```

### Keep curated entries, only add missing words

```bash
wikipa parse --lang fr   --preload exports/curated.dict.txt   --no-override   frwiktionary-latest-pages-articles.xml.bz2   > exports/fr.curated_plus_missing.dict.txt
```

### Let the new dump override an older dictionary

```bash
wikipa parse --lang fr   --preload exports/old.dict.txt   --replace   frwiktionary-20251120-pages-articles-multistream.xml.bz2   > exports/fr.override_old.dict.txt
```

---

## Notes

- `wikipa` is language‑agnostic as long as the dumps contain `{{pron|...}}` /
  `{{API|...}}` templates with the language code as one of the parameters.
- IPA detection uses the TIPA character set (`ipa.Charset`), which includes
  IPA and ExtIPA symbols, diacritics and suprasegmentals.
- Only parameters that **look like IPA** (contain at least one IPA character)
  and appear **before** the language code are kept as pronunciations.
