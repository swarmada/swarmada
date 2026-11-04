# Swarmada documentation voice rules

These rules encode the register the project publishes in. They are mechanical
checks over a subset of that register — passing them is necessary, not
sufficient.

| Rule | Level | What it enforces |
| :--- | :---- | :--------------- |
| `Marketing` | error | No promotional adjectives. A claim is a mechanism or a measurement. |
| `Difficulty` | error | No word that asserts a task is easy. It is not easy for the reader who is stuck. |
| `Neutrality` | error | No first person. Swarmada does things; the reader is `you`. |
| `Promises` | error | No commitment to unshipped functionality. Limits are stated as facts. |
| `Emoji` | error | None, in any documentation file. |
| `Exclamation` | error | None. Exclamation points read as sales copy or as panic. |
| `Ageing` | warning | No word whose truth depends on the day it was written. |
| `Hedges` | warning | No unquantified hedge. Assert the fact or state the condition. |

### Plain-language limits for normative text

The `Plain*` rules hold the normative text of the safety chapter
(`rfcs/rfc-0001-core-spec/safety.md`) and of two Fleet Adapter Protocol sections
("The Stop Latch" and "Connection lifecycle") to plain-language limits. They are
off for every other file. `.vale.ini` switches them on for the safety chapter;
Vale cannot scope a rule to a section, so the two FAP sections are extracted and
linted with the same rules by the maintainer-side wrapper.

| Rule | Level | What it enforces |
| :--- | :---- | :--------------- |
| `PlainKeywordPerSentence` | error | One RFC 2119 keyword per sentence (`MUST NOT` counts once). Each extra keyword is flagged. |
| `PlainSentenceLength` | error | At most 25 words per sentence. The 26th word is flagged. |
| `PlainExceptionPerSentence` | error | At most one of `unless`, `except`, `only if` per sentence. |
| `PlainParagraphSentences` | error | At most four sentences per paragraph or list item. |
| `PlainParagraphWords` | error | At most 80 words per paragraph or list item. |
| `PlainRefPerSentence` | error | At most one `{{ref:...}}` cross-reference per sentence. |
| `PlainActor` | error | Every requirement names its actor: no passive `MUST be sent` / `is to be kept` without `by`, and no sentence or list item that opens with the keyword. |
| `PlainInlineList` | error | Three or more conditions become bullets: a condition (`if`, `when`, `while`, `unless`, `once`) followed by a serial list of short items (`A, B, or C`), or a second `;` in one sentence. |
| `PlainRationale` | error | No rationale in normative text (`because`, `so that`, `in order to`, `this is why`, `the reason is`, causal `since`). Move it to a note. |
| `PlainDefinitionPhrase` | error | No term defined in passing (`is called`, `is defined as`, `means that`, `hereafter`, ...). |
| `PlainDefinitionBold` | error | No bolded term followed by `is`, `are` or `means` outside a Terms entry. |
| `PlainNoteShape` | error | A non-normative note is a blockquote of one paragraph (see below). |

What is checked, and how to write text the rules exempt:

- **Normative text** is every paragraph and every list item. Each list item is
  its own block for the paragraph limits, and its sentences are counted one by
  one. The sentence rules find sentence ends themselves: `.`, `?` or `!`
  followed by a word that does not start with a lowercase letter. A list lead
  and its nested items are separate blocks, so a lead is counted on its own.
- **A non-normative note** (rationale, example, status) is a blockquote of one
  paragraph, every line starting with `> `. For a note of several paragraphs,
  close each quote with an empty line that has no `>` and open a new `> `
  quote. A `>` line with nothing after it, a list, a nested quote or a code
  fence inside a quote is flagged by `PlainNoteShape`: the rules exempt only a
  quote's first block, and lint a second block as normative text.
- **A term** is defined once, in a Terms block: a bulleted list under a `Terms`
  heading or a `**Terms.**` lead, one entry per term, written
  `- **term**: definition`.
- **Tables, code blocks and headings** are not prose and are not checked. The
  sentence rules' scope excludes blockquotes, table cells and headings
  (`sentence & ~blockquote & ~table.cell & ~heading`), and the paragraph rules'
  scope does not reach them.
- **Vale version.** The rules are written for Vale 3.23. Vale 3.9 scoped
  blockquotes, table cells and list items differently; the maintainer-side
  wrapper (`plain-language/check.py`) refuses any other minor version.
- Inline code counts as one word, and so does a `{{ref:...}}`.

## Running the checks

```sh
go install github.com/errata-ai/vale/v3@v3.23.0
vale .
```

Errors fail CI. Warnings and suggestions do not, and are advisory during review.

## Register by surface

Neutrality tightens toward the specification:

| Surface | First person |
| :------ | :----------- |
| RFCs, specifications, API reference | Not permitted |
| README, guides, tutorials | Not permitted; address the reader as `you` |
| CLI, API, and controller messages | Not permitted |
| Signed blog posts and talk abstracts | Permitted, referring to the named authors |

The `blog/` and `talks/` paths are exempt from `Neutrality` in `.vale.ini`.
Nothing else is.

## Exemptions

`CODE_OF_CONDUCT.md` reproduces upstream text verbatim and is exempt in full.
`CHANGELOG.md` and `docs/roadmap.md` carry narrow exemptions recorded in
`.vale.ini`. Add an exemption by editing `.vale.ini` in a reviewed change, never
by rewording a rule to accommodate one document.

## Adding a term

Add the term to the rule file that already covers its category. A new rule file
needs a category that none of the rules above describes.
