#!/usr/bin/env bash
# Matrix item #7: box-drawing + emoji + CJK width fidelity.
#
# The failure this detects is *shearing*: if the emulator computes a different
# display width for a grapheme than the sender assumed, box borders stop lining
# up. Emoji are width-2, CJK is width-2, combining marks are width-0, and
# newer emoji (ZWJ sequences, flags) are where emulators disagree most.
#
# Run it locally first to get a reference, then through the proxy. Any
# difference between the two is a transport/terminfo problem. If both are
# equally broken, it's your emulator's width tables, not our bug.
#
#   ./render-test.sh                                    # local reference
#   go run ./client -url ws://localhost:8080/pty        # then run it inside
#
# Compare at two widths -- resize the window to ~80 cols, then ~200:
#   stty size

printf '\n=== 1. pure box-drawing (must be perfectly aligned) ===\n'
printf '┌────────────┬────────────┐\n'
printf '│ plain      │ plain      │\n'
printf '├────────────┼────────────┤\n'
printf '│ 1234567890 │ abcdefghij │\n'
printf '└────────────┴────────────┘\n'

printf '\n=== 2. width-2 emoji in fixed cells (borders must still align) ===\n'
printf '┌────────────┬────────────┐\n'
printf '│ 🚀🚀🚀🚀🚀 │ 🎉🎉🎉🎉🎉 │\n'
printf '│ ✅ done    │ ❌ failed  │\n'
printf '└────────────┴────────────┘\n'

printf '\n=== 3. CJK (each char is width 2) ===\n'
printf '┌────────────┬────────────┐\n'
printf '│ 日本語テスト │ 中文测试字符 │\n'
printf '└────────────┴────────────┘\n'

printf '\n=== 4. ZWJ sequences + flags + skin tone (emulators disagree here) ===\n'
printf '┌────────────┬────────────┐\n'
printf '│ 👨‍👩‍👧‍👦 family   │ 🇯🇵 flag     │\n'
printf '│ 👍🏽 skintone │ 🏳️‍🌈 rainbow  │\n'
printf '└────────────┴────────────┘\n'

printf '\n=== 5. combining marks (width 0 -- must not shift the border) ===\n'
printf '┌────────────┐\n'
printf '│ e\xcc\x81e\xcc\x81e\xcc\x81e\xcc\x81e\xcc\x81 acute │\n'
printf '└────────────┘\n'

printf '\n=== 6. truecolor gradient (item #8) ===\n'
for i in $(seq 0 31); do
  r=$((255 - i * 8)); g=$((i * 8)); b=128
  printf '\033[48;2;%d;%d;%dm ' "$r" "$g" "$b"
done
printf '\033[0m\n'

printf '\n=== 7. 256-color + attributes ===\n'
printf '\033[38;5;196mred\033[0m \033[1mbold\033[0m \033[3mitalic\033[0m \033[4munderline\033[0m \033[7mreverse\033[0m\n'

printf '\n=== 8. full-width ruler (check wrap at your terminal width) ===\n'
cols=$(tput cols 2>/dev/null || echo 80)
awk -v c="$cols" 'BEGIN{for(i=1;i<=c;i++) printf (i%10==0)?substr(i,length(i),1):((i%5==0)?"+":"-"); print ""}'
printf 'width reported by tput: %s\n' "$cols"

printf '\n--- PASS if: every ┌┬┐├┼┤└┴┘ column lines up vertically in sections 1-5,\n'
printf '    at BOTH 80 and 200 columns, and section 8 fills exactly one line.\n'
printf '    Sections 4-5 may legitimately fail in some emulators -- compare\n'
printf '    against the local reference run before blaming the proxy.\n\n'
