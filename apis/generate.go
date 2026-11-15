//go:build generate

// Code generation for the API types: deepcopy methods and CRD manifests by
// controller-gen, the crossplane-runtime method sets by angryjet. Run it with
// `make generate`; both tools are pinned as tool dependencies in go.mod.

//go:generate rm -rf ../package/crds
//go:generate go tool controller-gen object paths=./... crd:crdVersions=v1 output:artifacts:config=../package/crds
//go:generate go tool angryjet generate-methodsets ./...

package apis
