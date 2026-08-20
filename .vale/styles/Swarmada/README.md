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

## Running the checks

```sh
go install github.com/errata-ai/vale/v3@latest
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
needs a category that none of the eight above describes.
