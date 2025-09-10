// This file just exists as a place to put //go:generate directives that should apply to the entire project

package agentsandbox

//go:generate controller-gen crd:maxDescLen=0 paths="./..." output:crd:dir=k8s/crds
//go:generate controller-gen object paths="./..."
//go:generate nwa config -c add
