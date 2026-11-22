# address-refusal-isolation

A refusal of a named address describes the holder only to the project that owns it. Projects `alpha` and `beta` each reference `ari-blocks`, a shared class defined in `datum-cloud` and backed by `ari-root` (172.18.40.0/24). Beta holds 172.18.40.16/28. Alpha asking for 172.18.40.20 gets 409 `AddressInUse`, and the response must not contain beta's CIDR, beta's IPAllocation name, or beta's project. When alpha asks for an address inside its own block, the refusal still names that block and its IPAllocation.

Run it alone with `task e2e:suite SUITE=address-refusal-isolation`.
