# CI
PRs and merge queues run two Depot Linux jobs: formatting, code generation, vet,
vulnerability and cross-OS compile checks; then race tests, parser fuzz smoke,
and all nine deterministic simulation seeds. Each seed uses a fresh test run
on the same runner, avoiding 18 independent seed/OS setups.
Weekly and manual runs add the macOS job on depot-macos-latest and execute
benchmarks on both hosts. Native macOS regressions therefore no longer block
each PR. Main-push duplication is removed. No test seed was removed.
