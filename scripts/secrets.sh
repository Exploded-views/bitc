#!/bin/sh
# secrets.sh: scan the whole git history with gitleaks (run through scripts/dev.sh make secrets).
# Pass 1 uses the committed .gitleaks.toml. When GITLEAKS_EXTRA names a readable file, pass 2
# uses that file as its config. Output: the number of findings per pass and, per finding, the
# rule id, file and short commit. Never the match or the secret, not even redacted: gitleaks'
# own log goes to a file in a temp dir that is deleted, and only the summary is printed.
# Exit: 0 clean, 1 findings, 2 scanner or report error (fails closed).
set -u
d=$(mktemp -d) || exit 2
trap 'rm -rf "$d"' EXIT

# summarise LABEL REPORT: print the summary of a gitleaks JSON report; exit 1 on findings, 2 on a
# report that is not a complete JSON array.
summarise() {
  awk -v label="$1" '
    { buf = buf $0 "\n" }
    END {
      gsub(/^[ \t\r\n]+/, "", buf); gsub(/[ \t\r\n]+$/, "", buf)
      if (buf == "null") buf = "[]"
      if (substr(buf, 1, 1) != "[" || substr(buf, length(buf), 1) != "]") exit 2
      n = 0
      while (match(buf, /"(RuleID|File|Commit)"[ \t]*:[ \t]*"([^"\\]|\\.)*"/)) {
        tok = substr(buf, RSTART, RLENGTH); buf = substr(buf, RSTART + RLENGTH)
        key = tok; sub(/^"/, "", key); sub(/".*/, "", key)
        val = tok; sub(/^"[A-Za-z]+"[ \t]*:[ \t]*"/, "", val); sub(/"$/, "", val)
        if (key == "RuleID") { n++; rule[n] = val; file[n] = "-"; commit[n] = "-" }
        else if (n > 0 && key == "File") file[n] = val
        else if (n > 0) commit[n] = substr(val, 1, 12)
      }
      printf "secrets%s: %d findings\n", label, n
      for (i = 1; i <= n; i++) printf "  %s %s %s\n", rule[i], file[i], commit[i]
      exit (n > 0 ? 1 : 0)
    }' "$2"
}

# pass NAME LABEL CONFIG: one gitleaks run over every ref; returns 0, 1 (findings) or 2 (error).
pass() {
  if [ ! -r "$3" ]; then echo "secrets$2: config is not readable" >&2; return 2; fi
  rep=$d/$1.json
  # --exit-code 0: a finding is not an error, so any non-zero exit is a scanner error.
  if ! gitleaks git --redact --no-banner --log-opts=--all --config "$3" \
      --report-format json --report-path "$rep" --exit-code 0 >"$d/$1.log" 2>&1; then
    echo "secrets$2: scanner error" >&2; return 2
  fi
  if [ ! -s "$rep" ]; then echo "secrets$2: no report written" >&2; return 2; fi
  summarise "$2" "$rep"
  rc=$?
  if [ "$rc" = 2 ]; then echo "secrets$2: report is not a JSON array" >&2; fi
  return "$rc"
}

here=$(cd "$(dirname "$0")/.." && pwd)
worst=0
note() { # keep the worst code: error 2 beats findings 1 beats clean 0
  [ "$1" -gt "$worst" ] && worst=$1
  return 0
}
pass repo "" "$here/.gitleaks.toml"; note $?
if [ -n "${GITLEAKS_EXTRA:-}" ]; then
  pass extra " (extra)" "$GITLEAKS_EXTRA"; note $?
fi
exit "$worst"
