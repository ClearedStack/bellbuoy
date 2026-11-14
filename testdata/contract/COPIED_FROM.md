# Where these files came from

Everything else in this folder is a copy of `contract/v1/examples/` from the
Muster repository, branch `claude/repo-build-prompt-1csgx6`, commit
`a362cb1051448d496b5b97ff808ebda60370502f`. Muster's copy is the master. Do
not edit the copies here: copy the folder again when Muster's contract
changes, and run the tests.

`internal/muster/contract_test.go` replays every example that is one of the
five worker calls. It checks that Bellbuoy's client sends exactly the request
in the example, and that it reads the answer in the example the way the
contract means it.
