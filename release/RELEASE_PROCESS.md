# Release Process

## Create an issue for release tracking

- Create an issue in [kserve/modelmesh-serving](https://github.com/kserve/modelmesh-serving)
- Label the issue with `priority p0`
- Label the issue with `kind process`

## Releasing KServe components

A release branch should be substantially _feature complete_ with respect to the intended release.
Code that is committed to `main` may be merged or cherry-picked on to a release branch, but code that is directly committed to the release branch should be solely applicable to that release (and should not be committed back to main).
In general, unless you're committing code that only applies to the release stream (for example, temporary hotfixes, backported security fixes, or image hashes), you should commit to `main` and then merge or cherry-pick to the release branch.

## Create a release branch

If you aren't already working on a release branch (of the form `release-${MAJOR}`, where `release-${MAJOR}` is a major-minor version number), then create one.
Release branches serve several purposes:

1.  They allow a release wrangler or other developers to focus on a release without interrupting development on `main`,
1.  They allow developers to track the development of a release before a release candidate is declared,
1.  They simplify back porting critical bug fixes to a patch level release for a particular release stream (e.g., producing a `v0.6.1` from `release-0.6`), when appropriate.

## Publish the release

It's generally a good idea to search the repo or control-f for strings of the old version number and replace them with the new, keeping in mind conflicts with other library version numbers.

1. Update `modelmesh`, `modelmesh-runtime-adapter`, and `modelmesh-controller` image tags to the corresponding release version numbers.
   - Edit `newTag` in `config/manager/kustomization.yaml`.
   - Edit the the `modelmesh` and `modelmesh-runtime-adapter` image tags in `config/default/config-defaults.yaml`.
1. Submit your PR to the release branch and wait for it to merge.
1. Generate release manifests:
   - `kustomize build config/default > modelmesh.yaml`
   - `kustomize build config/runtimes --load-restrictor LoadRestrictionsNone > modelmesh-runtimes.yaml`
   - `cp config/dependencies/quickstart.yaml modelmesh-optional-dependencies.yaml`
1. Once everything has settled, tag and push the release with `git tag $VERSION` and `git push upstream $VERSION`. You can also tag the release in the GitHub UI.
   - The `modelmesh-controller` image will be published via GitHub Actions.
1. Upload generated install manifests to GitHub release assets.
