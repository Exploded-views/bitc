## What and why

<!-- What changes, and what problem it solves. Link the issue if there is one (Refs #N). -->

## How it was tested

<!-- Commands you ran, or the test that now covers it. -->

## Checklist

- [ ] A test came first and was seen failing (`Failing first:` line in the commit message)
- [ ] Every new or changed behaviour has a row in `conformance/behaviours.toml` and a `Test<ID>_` test
- [ ] `make ci` passes (`scripts/dev.sh make ci` without a local Go toolchain)
- [ ] Docs and `CHANGELOG.md` (under Unreleased) are updated where users would notice
- [ ] No secrets, personal data or new dependencies
