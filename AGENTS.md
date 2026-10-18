# Agent guide for omp-operator

Kubebuilder v4 project (controller-runtime v0.25, Go 1.27). Tools are pinned in `mise.toml`;
every repo command is a mise task (`mise tasks`). There is no Makefile.

- `mise run generate` / `manifests` / `chart-sync`: regenerate DeepCopy, CRDs, RBAC and the chart's
  `crds/` and `files/rules.yaml`. Never hand-edit generated files.
- `mise run ci`: lint, unit + envtest + chart tests, `helm lint`. `mise run verify-generated` after
  committing.
- `mise run ompcompat`: tests against the pinned real omp. `mise run test-e2e`: kind e2e.
- API types live in `api/v1alpha1`; after editing them run `mise run chart-sync`.
- RBAC: cluster-scoped rules are `+kubebuilder:rbac` markers; namespaced rules live in the chart.
- Helm (`charts/omp-operator`) is the only install path; `config/` holds generated CRDs, RBAC and
  samples only.
- User docs are `README.md` and `docs/` (reference tables under `docs/reference/`). They describe the
  code, not the plan: when you change a flag, chart value, CRD field, default, limit or HTTP behavior,
  update the page that names it in the same change. Package map: `docs/development.md`.
