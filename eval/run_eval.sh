#!/usr/bin/env bash
# Cortex recall eval — queries 100 questions, scores top-5 results by keyword hit-rate
# Usage: bash eval/run_eval.sh
#   LIMIT=5  (override results per query)
#   JSON_OUT=eval/results.json (override output path)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CORTEX="$REPO_ROOT/cortex"
QUESTIONS="$SCRIPT_DIR/questions.json"

LIMIT="${LIMIT:-5}"
JSON_OUT="${JSON_OUT:-$SCRIPT_DIR/results.json}"

if [[ ! -x "$CORTEX" ]]; then
  echo "ERROR: $CORTEX not found. Run: go build -o cortex ./cmd/cortex" >&2
  exit 1
fi

total=$(jq 'length' "$QUESTIONS")
echo "Running $total questions (limit=$LIMIT per query)..."
echo ""

pass=0; fail=0; empty=0
# Build results array incrementally into a temp file
tmp_results="$(mktemp)"
echo "[]" > "$tmp_results"

for i in $(seq 0 $((total - 1))); do
  row=$(jq -c ".[$i]" "$QUESTIONS")
  id=$(echo  "$row" | jq -r '.id')
  q=$(echo   "$row" | jq -r '.q')

  # Run recall; capture stdout, tolerate non-zero exit
  raw=$("$CORTEX" recall "$q" --limit "$LIMIT" --json 2>/dev/null || echo '{"Results":[]}')

  result_count=$(echo "$raw" | jq '.Results | length' 2>/dev/null || echo 0)

  if [[ "$result_count" -eq 0 ]]; then
    verdict="EMPTY"; score="0.00"
    matched_kw=""; missed_kw=$(echo "$row" | jq -r '.kw | join(",")'); top_bodies="[]"
    empty=$((empty + 1))
  else
    # Concatenate all bodies, lowercase
    bodies_lower=$(echo "$raw" | jq -r '[.Results[]?.Body // ""] | join(" ")' | tr '[:upper:]' '[:lower:]')

    hit_count=0; kw_total=0
    matched_list=(); missed_list=()
    while IFS= read -r kw; do
      kw_total=$((kw_total + 1))
      if echo "$bodies_lower" | grep -qiF "$kw"; then
        hit_count=$((hit_count + 1))
        matched_list+=("$kw")
      else
        missed_list+=("$kw")
      fi
    done < <(echo "$row" | jq -r '.kw[]')

    score=$(awk "BEGIN { printf \"%.2f\", $hit_count / $kw_total }")
    matched_kw=$(IFS=","; echo "${matched_list[*]:-}")
    missed_kw=$(IFS=",";  echo "${missed_list[*]:-}")

    if awk "BEGIN { exit !($score >= 0.6) }"; then
      verdict="PASS"; pass=$((pass + 1))
    else
      verdict="FAIL"; fail=$((fail + 1))
    fi

    top_bodies=$(echo "$raw" | jq '[.Results[:3][]?.Body // ""]' 2>/dev/null || echo "[]")
  fi

  pad_id=$(printf "%3d" "$id")
  printf "[%s] #%s  score=%-4s  q=%s\n" "$verdict" "$pad_id" "$score" "$q"
  if [[ "$verdict" != "PASS" ]]; then
    [[ -n "$missed_kw" ]] && printf "         missed: %s\n" "$missed_kw"
  fi

  # Append entry to running results array
  entry=$(jq -n \
    --argjson id   "$id" \
    --arg     q    "$q" \
    --arg     v    "$verdict" \
    --arg     sc   "$score" \
    --arg     mk   "$matched_kw" \
    --arg     msk  "$missed_kw" \
    --argjson tb   "$top_bodies" \
    '{id:$id,q:$q,verdict:$v,score:($sc|tonumber),matched_kw:$mk,missed_kw:$msk,top_bodies:$tb}')

  jq --argjson e "$entry" '. + [$e]' "$tmp_results" > "${tmp_results}.new"
  mv "${tmp_results}.new" "$tmp_results"
done

overall=$(awk "BEGIN { printf \"%.1f\", $pass / $total * 100 }")
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "RESULTS: $pass pass / $fail fail / $empty empty   total=$total"
echo "PASS RATE: ${overall}%"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

jq \
  --argjson pass  "$pass" \
  --argjson fail  "$fail" \
  --argjson empty "$empty" \
  --argjson total "$total" \
  --arg     rate  "${overall}%" \
  '{summary:{pass:$pass,fail:$fail,empty:$empty,total:$total,pass_rate:$rate},questions:.}' \
  "$tmp_results" > "$JSON_OUT"

rm -f "$tmp_results"
echo "Full report: $JSON_OUT"
