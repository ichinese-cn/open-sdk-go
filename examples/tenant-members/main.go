// Lists an institution's members with its tenant token, page after page.
//
//	APP_ID=… APP_SECRET=… TENANT_KEY=… [OPEN_API_BASE=…] go run ./examples/tenant-members
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	opensdk "github.com/ichinese-cn/open-sdk-go"
)

func main() {
	var opts []opensdk.Option
	if base := os.Getenv("OPEN_API_BASE"); base != "" {
		opts = append(opts, opensdk.WithBaseURL(base))
	}
	client := opensdk.NewClient(os.Getenv("APP_ID"), os.Getenv("APP_SECRET"), opts...)
	tenantKey := os.Getenv("TENANT_KEY")
	ctx := context.Background()

	tenant, err := client.GetTenant(ctx, opensdk.WithTenantKey(tenantKey))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("institution: %v\n", tenant["name"])

	type member struct {
		OpenID string `json:"open_id"`
		Name   string `json:"name"`
		Role   string `json:"role"`
	}
	n := 0
	for item, err := range client.ListTenantMembersAll(ctx, opensdk.Params{"page_size": 50}, opensdk.WithTenantKey(tenantKey)) {
		if err != nil {
			if opensdk.IsScopeError(err) {
				// The institution has not granted org:member:readonly: do without the directory.
				log.Printf("no access to the directory: %v", err)
				return
			}
			log.Fatal(err)
		}
		m, err := opensdk.Decode[member](item)
		if err != nil {
			log.Fatal(err)
		}
		n++
		fmt.Printf("%s\t%s\t%s\n", m.OpenID, m.Name, m.Role)
	}
	fmt.Printf("%d members\n", n)
}
