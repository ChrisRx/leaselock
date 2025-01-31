# leaselock

leaselock is a distributed mutex facilitated by the Kubernetes Lease API.

## Usage

Running the following program in a `Deployment`, with more than 1 replica, will result in only one pod running the user-provided function at any given time:

```go
package main

import (
	"context"
	"fmt"
	"os"
    "log"

	"go.chrisrx.dev/leaselock"
)

func main() {
	if err := leaselock.Run(context.TODO(), func(ctx context.Context, l *leaselock.LeaseLock) error {
		fmt.Printf("%s: I'm the leader!", l)
		return nil
	}, leaselock.Options{
		Name:      "my-test-lock",
		Namespace: "my-namespace",
		ID:        os.Getenv("POD_NAME"),
	}); err != nil {
		log.Fatal(err)
	}
}
```


### Kubernetes Lease API

This library uses [leaderelection](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection), which utilizes the Kubernetes Lease API to coordinate leader election. Here is an example `Role`/`RoleBinding` showing the API access leaselock needs to use the Lease API:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: leases
  namespace: my-namespace
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "create", "update"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  resourceNames: ["my-test-lock"]
  verbs: ["delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: leases
  namespace: my-namespace
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: leases
subjects:
- kind: ServiceAccount
  name: default
  namespace: my-namespace
```

This `Role` also specifies the resource name of the lock to further reduce the scope, but isn't necessary.
