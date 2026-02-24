package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
)

// Scheme is the runtime scheme shared by all Janus binaries.
var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(backupv1alpha1.AddToScheme(Scheme))
}
