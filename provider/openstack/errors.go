// Copyright 2018 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package openstack

import (
	gooseerrors "gopkg.in/goose.v2/errors"

	"github.com/juju/juju/environs/context"
	)

func MaybeHandleCredentialError(err error, ctx context.ProviderCallContext) (error, bool) {
	IsUnauthorized := gooseerrors.IsUnauthorised(err)
	//fmt.Printf("XXXXX... IsUnauthorized : %v\n", IsUnauthorized)
	if ctx != nil && IsUnauthorized {
		invalidateErr := ctx.InvalidateCredential("openstack cloud denied access")
		if invalidateErr != nil {
			logger.Warningf("could not invalidate stored openstack cloud credential on the controller: %v", invalidateErr)
		}
	}
	return err, IsUnauthorized
}

// HandleCredentialError determines if a given error relates to an invalid credential.
// If it is, the credential is invalidated. Original error is returned untouched.
func HandleCredentialError(err error, ctx context.ProviderCallContext) error {
	MaybeHandleCredentialError(err, ctx)
	return err
}
