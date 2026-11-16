# Changelog

## [0.1.1](https://github.com/fluencelabs/crossplane-provider-rauthy/compare/v0.1.0...v0.1.1) (2026-11-16)


### Bug Fixes

* **client:** scopes and default scopes cannot be empty ([699ee6d](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/699ee6d1a9b52869f177fbe6bfc15653822b61cd))
* **client:** scopes and default scopes cannot be empty ([b4c18b4](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/b4c18b4b59d527792cb55e650df98e5e6d850e26))

## 0.1.0 (2026-11-15)


### Features

* API types for Client and ClusterProviderConfig ([0095987](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/0095987844baf8e6713d03ea2432c823b059a11d))
* bind the ownership record to the client secret ([c3ab4b7](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/c3ab4b7a77c8d02ff027c5f89e19f946f65e7bcb))
* Client controller ([792cd62](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/792cd62d382dd5bb71510f372e2afebc0c96853b))
* **client:** build on rauthy-go v0.1.0 and keep unmanaged client fields on update ([6a0c8ee](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/6a0c8eefbf31a2d753294da63b21b0b233514f9c))
* **client:** build on rauthy-go v0.1.0 and keep unmanaged client fields on update ([8a7e77a](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/8a7e77a4d078e872b6561fbc0f7c4eb4ec23b86d))
* **client:** let operators find the Client holding a blocking record ([b3337e8](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/b3337e8e6641b3e7190e62a2c0a6b96675dc1c80))


### Bug Fixes

* **client:** clear optional fields set to an empty list or string ([b6db8e5](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/b6db8e5440b46b2894a9b3b97f666b43afddb1e7))
* **client:** detect a CRD that prunes status.ownership from what the server stored ([70c8653](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/70c8653d8c5b836af14daadbbf289bfb07bb826e))
* **client:** refuse an id another Client records, in any namespace ([995782d](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/995782df487e9be3b180b14ca8203d5d6278a5ec))
* **client:** take the whole status patch answer into the reconciled object ([e6bbbd4](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/e6bbbd4f23c64f2b7332db911888ced59ec50f9e))
* confirm client ownership against the API server before acting ([ba0ad02](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/ba0ad029122fa444408b195d5a2ba0783d7b70db))
* keep the ownership record and the Rauthy client in step ([11cd558](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/11cd558a091f752c6cf6308b431287c0e2966fa8))
* never clear or release a record that names another client id ([0a90627](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/0a9062729e55ce206623ba8f89ef305787956ee4))
* **provider:** keep Secrets out of the manager cache ([3804e88](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/3804e88312fac97a9d22f83802d468610f80fb8a))
* record client ownership in the status subresource ([cc6542b](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/cc6542bedacece123c3a49a866b847cfaa2e6a38))
* record client ownership in the status subresource ([0523835](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/0523835a4e5acb22b8397636bde7867a0c239cdc))
* refuse a client whose secret does not match the ownership record ([aa6392a](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/aa6392aed466beb4b67afd9c9a34d66ebece30b2))


### Performance Improvements

* **client:** look other ownership records up in the cache on observe ([0835461](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/0835461e67820314fa7f08d51ead7cd4f7f3a242))
