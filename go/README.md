# Actions

Executable for repository.

## Feature

- Reconcile GitHub repos, labels, milestones, releases, branches, and LFS objects to Codeberg, CNB, and Forgejo targets; failures are logged and skipped, never failing the run.
- Mirror arbitrary upstream git repositories to any git target (defaults to GitHub); mirror clones are cached between runs and refreshed incrementally, missing GitHub targets are created, and credentials are injected as HTTP Authorization headers, never embedded in remote URLs. Mirror failures are logged and skipped, never failing the run. LFS objects are not mirrored.

## Credits

- [cnb/plugins/cnbcool/code-import](https://cnb.cool/cnb/plugins/cnbcool/code-import) (MIT)
- [cnb/cool/git-clone-yyds](https://cnb.cool/cnb/cool/git-clone-yyds) (MIT)
- [cnb/plugins/tencentcom/git-reconcileBranch](https://cnb.cool/cnb/plugins/tencentcom/git-reconcileBranch) (MIT)
