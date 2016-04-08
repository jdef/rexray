/*
Copyright (c) 2015 VMware, Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fields

import (
	"flag"

	"github.com/vmware/govmomi/govc/cli"
	"github.com/vmware/govmomi/govc/flags"
	"github.com/vmware/govmomi/object"
	"golang.org/x/net/context"
)

type rm struct {
	*flags.ClientFlag
}

func init() {
	cli.Register("fields.rm", &rm{})
}

func (cmd *rm) Register(f *flag.FlagSet) {}

func (cmd *rm) Process() error { return nil }

func (cmd *rm) Usage() string {
	return "KEY..."
}

func (cmd *rm) Run(f *flag.FlagSet) error {
	ctx := context.TODO()

	c, err := cmd.Client()
	if err != nil {
		return err
	}

	m, err := object.GetCustomFieldsManager(c)
	if err != nil {
		return err
	}

	for _, name := range f.Args() {
		key, err := m.FindKey(ctx, name)
		if err != nil {
			return err
		}

		if err := m.Remove(ctx, key); err != nil {
			return err
		}
	}

	return nil
}
