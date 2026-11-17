# Changelog

## [0.2.0](https://github.com/fluencelabs/crossplane-provider-rauthy/compare/v0.1.1...v0.2.0) (2026-11-17)


### Features

* **client:** a public client must keep S256 among its PKCE methods ([4b3ae36](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/4b3ae366917ab6fdeb2ee927c5eb25983b4f0770))
* **client:** a public client must keep S256 among its PKCE methods ([f973004](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/f9730045c86231ff65fb770745e04557944667f6))


### Bug Fixes

* align client constraints and bind ownership to Rauthy ([1869174](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/1869174f66ec2db783869212b8d67af980effc78))
* align client constraints with Rauthy and bind ownership to the Rauthy instance ([02b3ad7](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/02b3ad70f238c2bd874cf8e7b30ebf42aa834858))
* **client:** bind ownership records from earlier releases to the current backend ([22887df](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/22887dfd5fe355c7f4bd23710ebe805ac50bddf8))
* **client:** clear Ready whenever Observe refuses or fails ([35afcb8](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/35afcb8d6f6301c04a57ad6d76b5c64924af5881))
* **client:** confirm ownership right before every write ([55ebeb4](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/55ebeb4ce401cb378f34b6df05debf6921e37722))
* **client:** confirm ownership right before every write ([7ed7838](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/7ed783803c0e7e92e0250509fb436365a3d20a5c))
* **client:** exclude credentials from ownership backend identity ([da7214c](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/da7214cac3318a81a07f3150ef948eedbaeee111))
* **client:** keep credentials out of the ownership backend identity ([2b565b0](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/2b565b03086b6cdfaee5872b3bea170e4252caec))
* **client:** keep the creation proof until a late POST is settled ([e8dff1d](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/e8dff1daf0179e9972fc6b36a801dca002dbdee0))
* **client:** name the renamed backend field in the stale CRD check ([4b49a68](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/4b49a68b8fb68b6c49abca6e054ff7f54c998bc1))
* **client:** no SDK replays after the ownership check; check ownership before rollback ([23d0f4b](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/23d0f4b994dec48d1156898b194e5dc9ec01c164))
* **client:** publish the issuer exactly as discovery reports it ([bc2ccac](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/bc2ccacc2cd17fc21b358ce562a6aa0aad17b869))
* **client:** publish the issuer exactly as discovery reports it ([461952b](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/461952b65838303cbbf3f786bcf4bf105aaeff5a))
* **client:** record a fingerprint of the Rauthy backend, not its URL ([d643144](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/d643144b98c95f34ec6aee217500f75ec5e4ca26))
* **client:** recover concurrent creation and normalize empty names ([297bd8c](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/297bd8c5924f05b722983fbed66817de3edc8161))
* **client:** recover pending creation after transient failures ([114ded4](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/114ded4768891349df1c9c43ae3ba7173c48e978))
* **client:** refuse a public client whose observed PKCE methods lack S256 ([89260a8](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/89260a8c6ac50f308548007e5184825b5fa7de9c))
* **client:** refuse comma redirect URIs in the controller; a CEL rule over the list breaks CRD install ([1471361](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/1471361ea630e6714041ff53b254e6e49a598694))
* **client:** refuse commas in redirect URIs, name a stale CRD, tighten normalization tests ([cacb4f6](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/cacb4f65d521b17e38cfe0d79e7370755e5f6a9e))
* **client:** reject comma redirects and identify stale CRDs ([7dde186](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/7dde1863865b235d1b8c5838ac87d558d37907f6))
* **client:** reject unsafe observed public PKCE methods ([fd2024e](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/fd2024eab159498ff625abfb070ef0a9f18adc38))
* **client:** report an unparseable backend URL from Connect instead of panicking ([c5815e5](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/c5815e5f12e666a2675fbd971f90ca476c368392))
* **client:** retain creation proof across late POST commits ([bd78499](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/bd78499a0e1f2bd1ce9ec7cbb3df9034bff79419))
* **client:** unbound legacy records block other owners; split scope CSV before comparing ([bb1b5f6](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/bb1b5f64527404da96769a5f7e644b66fd677eeb))
* confirm client ownership before each write ([3a0c81d](https://github.com/fluencelabs/crossplane-provider-rauthy/commit/3a0c81d3e6a0f38dac4b0d265db38f0ca71abf9a))

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
