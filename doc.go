// Package microvm provides a small convenience layer over AWS Lambda MicroVMs.
//
// The package intentionally keeps the AWS SDK for Go v2 as the control-plane
// implementation and focuses on the common workflow:
// package an fs.FS as a MicroVM artifact, upload it to S3, create a MicroVM
// image, run a MicroVM from that image, create an endpoint auth token, and send
// authenticated HTTP requests to the MicroVM endpoint.
package microvm
