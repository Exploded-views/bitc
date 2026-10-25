#!/bin/sh
# linkcheck.sh: every relative markdown link in the repository's tracked *.md files must resolve to
# an existing file or directory. Checks `](path)`, `](path#anchor)` and `](path?query)`; the anchor
# is not checked, and %20 is decoded. Skips http(s), mailto and pure-anchor links, and links inside
# fenced code blocks (``` or ~~~).
#
# A directory holding a file named .linkcheck-skip is private to the development repository and
# is not part of the public tree. Its own files are not checked, and a link from any other file
# into it is reported as broken, because the link would dangle in the public tree.
#
# Prints `linkcheck: clean`, or each broken `file: target`, and exits 1.
set -eu

cd "$(dirname "$0")/.."

bad=$(mktemp)
trap 'rm -f "$bad"' EXIT

# normalise PATH lexically: drop empty and . parts, fold .. ; prints nothing if it escapes the root
norm() {
	printf '%s\n' "$1" | awk -F/ '{
		n = 0
		for (i = 1; i <= NF; i++) {
			if ($i == "" || $i == ".") continue
			if ($i == "..") { if (n == 0) { print "../"; exit } n--; continue }
			p[++n] = $i
		}
		out = ""
		for (i = 1; i <= n; i++) out = out (i > 1 ? "/" : "") p[i]
		print out
	}'
}

skips=$(git ls-files -- '*/.linkcheck-skip' '.linkcheck-skip' | while IFS= read -r m; do
	d=$(dirname "$m")
	[ "$d" = . ] || echo "$d"
done)

under_skip() { # PATH: is it inside a skip directory?
	for s in $skips; do
		case "$1" in "$s" | "$s"/*) return 0 ;; esac
	done
	return 1
}

git ls-files -- '*.md' | while IFS= read -r f; do
	under_skip "$f" && continue
	dir=$(dirname "$f")
	awk '
		/^[ \t]*(```|~~~)/ { fence = !fence; next }
		fence { next }
		{
			line = $0
			while (match(line, /\]\([^)]*\)/)) {
				print substr(line, RSTART + 2, RLENGTH - 3)
				line = substr(line, RSTART + RLENGTH)
			}
		}' "$f" |
	while IFS= read -r target; do
		target=${target%% *} # drop an optional "title"
		case "$target" in
		'' | '#'* | http://* | https://* | mailto:*) continue ;;
		esac
		path=${target%%#*}
		path=${path%%\?*}
		[ -n "$path" ] || continue
		path=$(printf '%s' "$path" | sed 's/%20/ /g')
		case "$path" in
		/*) full=${path#/} ;;
		*) full="$dir/$path" ;;
		esac
		full=$(norm "$full")
		if [ "$full" = "../" ] || [ ! -e "$full" ]; then
			echo "$f: $target" >>"$bad"
		elif under_skip "$full"; then
			echo "$f: $target (not part of the public tree)" >>"$bad"
		fi
	done
done

if [ -s "$bad" ]; then
	sort -u "$bad"
	exit 1
fi
echo "linkcheck: clean"
