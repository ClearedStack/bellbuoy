# Where these files came from

`forge_exchange.json` in this folder is an unmodified copy of
`contract/v1/examples/forge_exchange.json` from `ClearedStack/plainforge`,
branch `main`, commit `6827c531054f105ebcccba300e97812a747b9334`. Plainforge's
copy is the master. Do not edit the copy here: copy the file again when
Plainforge's Forge HTTP Contract v1 (`contract/v1/forge.md`) changes, and run
the tests.

`internal/plainforge/contract_test.go` replays all 15 saved examples in
`forge_exchange.json`: the five `Forge` calls end to end, exact request method,
URL and JSON body checks, idempotent change replay (`201` vs `200`), and every
refusal status and body with human message text stripped.
