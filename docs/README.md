# Documentation development

`oxide-karpenter.adoc` is the only publishable documentation source. The Oxide website build can fetch that one file and render it with the Oxide design system.

The file is generated from two sources:

- `templates/asciidoctor/oxide-karpenter.tpl` contains the short overview and example.
- The Go types and comments under `pkg/apis/v1alpha1` produce the field-level `OxideNodeClass` reference.

Run the generator after changing either source:

```console
make docs-generate
```

Commit the resulting `oxide-karpenter.adoc` so the website repository can fetch it without running provider tooling. Do not edit it directly. `OxideCapacity` is intentionally excluded.

## Local preview

The preview uses the published `@oxide/design-system` package and has no sibling-repository dependency.

```console
npm ci --prefix docs/site
make docs-serve
```

Build the static site into the ignored `docs/dist` directory with `make docs`.

## GitHub Pages and pull request previews

The `.github/workflows/docs.yml` workflow builds every documentation pull request, publishes `main` at the Pages root, and publishes same-repository pull requests below `pr-preview/pr-<number>/`. It removes a preview when its pull request closes.

Enable it once after merging the workflow:

1. Under **Settings → Actions → General → Workflow permissions**, choose **Read and write permissions**.
2. Run the **Documentation** workflow once from `main` so it creates the `gh-pages` branch.
3. Under **Settings → Pages**, choose **Deploy from a branch**, select `gh-pages`, and use the root directory.

Fork pull requests receive the build check but do not publish a preview because their workflow token is read-only.
