# CI workflow

`ci.yml.example` is the intended GitHub Actions workflow (gofmt check, `go vet`,
`go test -race`, and a cross-compile matrix).

It is not under `.github/workflows/` yet because the token used for the initial
push lacked the `workflow` OAuth scope. To enable it:

```sh
gh auth refresh -s workflow -h github.com     # one-time, grants the scope
mkdir -p .github/workflows
git mv .github/ci.yml.example .github/workflows/ci.yml
git commit -m "Enable CI workflow"
git push
```
