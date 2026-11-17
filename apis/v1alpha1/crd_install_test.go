package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	internalapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"sigs.k8s.io/yaml"
)

// Every generated CRD passes the API server's own validation of a CRD, which
// includes the CEL cost budget: admission tests of objects cannot see a CRD
// the API server would refuse to install.
func TestCRDsInstall(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "package", "crds", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find the CRDs: %v (%d files)", err, len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			for _, e := range validateCRD(t, file) {
				t.Error(e)
			}
		})
	}
}

// validateCRD runs the API server's CRD validation on the CRD in file.
func validateCRD(t *testing.T, file string) []error {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err = yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&crd)
	var internal internalapi.CustomResourceDefinition
	if err = apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(
		&crd, &internal, nil,
	); err != nil {
		t.Fatal(err)
	}
	for _, v := range internal.Spec.Versions {
		if v.Storage {
			internal.Status.StoredVersions = []string{v.Name}
		}
	}
	errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &internal)
	if len(errs) == 0 {
		return nil
	}
	return errs.ToAggregate().Errors()
}
