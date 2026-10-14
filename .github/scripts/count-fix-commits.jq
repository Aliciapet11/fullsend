# Prior fix-agent commits among the pages of `gh api .../pulls/N/commits
# --paginate`, slurped with -s: authored as fullsend-fix, or published through
# the GitHub API with that identity in an Authored-by trailer in the last
# paragraph (fullsend-ai/agents#1675). No input at all is an error, so the
# caller falls back to the cap.
add
| map(select(.commit.author.name == "fullsend-fix"
    or any((.commit.message // "") | sub("\\s+$"; "") | split("\n\n") | (last // "") | split("\n")[];
           startswith("Authored-by: fullsend-fix <"))))
| length
