# Where these files came from

Everything else in this folder is a copy of `contract/v1/examples/` from the
Muster repository, branch `claude/repo-build-prompt-1csgx6`, commit
`470dddfe8410738052655313bd32fbeefc715f9f`. Muster's copy is the master. Do
not edit the copies here: copy the folder again when Muster's contract
changes, and run the tests.

`internal/muster/contract_test.go` replays every saved `.json` example in this
folder: the five worker calls end to end, every refusal status and body against
Bellbuoy's HTTP client, and every non-worker call against Bellbuoy's contract
boundary.
