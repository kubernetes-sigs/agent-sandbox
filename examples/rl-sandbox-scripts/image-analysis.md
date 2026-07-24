# SWE-bench image analysis (R2E-Gym/SWE-Bench-Verified)

Working notes (local). Grounds the disk / pre-pull / sizing discussion with real
numbers. Data sources: HF datasets-server (full 500-row composition) and Docker
Hub web API (`hub.docker.com`, tag `full_size`, 300-tag sample). Layer-byte
breakdown is inferred from our measured pull times (registry manifest reads are
rate-limited right now — see "To measure next").

## TL;DR

- **500 images, 12 repo families.** Heavily skewed: **django = 231 (46%)**.
- **Per-tag size:** min 0.96 GB, **median ~1.15 GB**, mean ~1.51 GB, max 3.47 GB
  (Docker Hub `full_size` = *compressed*; on-disk uncompressed is ~2–2.5×).
- **All 500 resident = ~720–755 GB compressed (~1.5–1.9 TB on disk).** So you
  cannot hold the whole set on a node — and the user's "~650 GB" is actually a
  slight *under*-estimate.
- **But you never need to.** Disk = **sum of unique layers** (family base stored
  once + thin per-instance diff), and a node only needs its **working set** (the
  active sliding window). Size disk to the window, not the dataset.

## 1. Dataset composition (all 500)

| Repo family | Tasks | % of 500 | Median image (compressed) |
| :--- | --: | --: | --: |
| django/django | 231 | 46.2% | ~1.34 GB |
| sympy/sympy | 75 | 15.0% | ~1.15 GB |
| sphinx-doc/sphinx | 44 | 8.8% | ~1.10 GB |
| matplotlib/matplotlib | 34 | 6.8% | **~3.47 GB** |
| scikit-learn/scikit-learn | 32 | 6.4% | ~1.55 GB |
| pydata/xarray | 22 | 4.4% | ~2.08 GB |
| astropy/astropy | 22 | 4.4% | ~1.20 GB |
| pytest-dev/pytest | 19 | 3.8% | ~0.99 GB |
| pylint-dev/pylint | 10 | 2.0% | ~1.05 GB |
| psf/requests | 8 | 1.6% | ~0.96 GB |
| mwaskom/seaborn | 2 | 0.4% | ~1.24 GB |
| pallets/flask | 1 | 0.2% | ~1.09 GB |
| **Total** | **500** | 100% | mean ~1.51 GB |

Key implications of the skew:
- **One family (django) is ~half the workload.** Pre-pulling/streaming django's
  base layer benefits 231 tasks — the highest-leverage single action.
- The **12 family base layers** are the bulk of resident bytes; the 500
  per-instance diffs are comparatively small.
- **matplotlib (3.47 GB) and xarray (2.08 GB)** are the heavy families — they
  dominate disk per-image and are worth isolating/streaming.

## 2. Size distribution (300-tag sample)

```
min     0.96 GB
median  1.15 GB
mean    1.51 GB     (skewed up by matplotlib/xarray)
max     3.47 GB
```
`full_size` is the **compressed** registry size (sum of layer blobs). On the node
the image store is **uncompressed**, typically **~2–2.5×** larger, and that is
what counts against node disk.

## 3. Layer-sharing model (why naive Σ over-counts)

SWE-bench images are built per-repo: a shared **base layer** (OS + toolchain +
the repo's deps at a pinned commit) plus a thin **top layer** (the
instance-specific checkout/patch). On a node, identical layers are stored **once**:

```
node_disk(set) ≈ Σ_family(base_layer_family)            # one per family present
              + Σ_instance(diff_layer_instance)         # small, per image
```

**Empirical evidence (our measured runs, see performance.md):**
- Fresh family, cold (django, nothing shared on node): warm pull **~81 s**.
- Same family, base already cached (astropy 2nd image): warm pull **~11 s**.
- ⇒ the base dominates pull time/bytes; the per-instance diff is small (~order of
  10–15% of the work here).

So the true resident footprint of "all 500" is **far below** the ~750 GB naive
sum — most of the 500 collapse onto 12 bases. (Exact base-vs-diff bytes per
family still to be measured.)

## 4. Disk implications

### Naive "everything on every node" — infeasible
500 × ~1.51 GB ≈ **~755 GB compressed**, ~1.5–1.9 TB on disk. Far past a 100 GB
node, and pointless (a node only runs a subset at a time).

### Working-set model — what you actually size for
A node only needs images for the pods scheduled on it *now* = the active sliding
window. Budget:

```
disk_per_node ≳ system_overhead
             + Σ_active_families(base_uncompressed)      # ~2.5× compressed base
             + window_instances × diff_uncompressed
             + GC_headroom (~15%: kubelet high/low thresholds)
```

**Worked examples** (rough, using ~1.2 GB compressed median ⇒ ~3 GB on-disk
full image; assume base ≈ 85% shared, diff ≈ 15%):

| Active working set | On-disk estimate | Fits 100 GB node? |
| :--- | --: | :--: |
| 1 family, window 8 instances | ~3 GB base + 8×0.45 ≈ **~6.6 GB** | ✅ easily |
| 3 families, window 24 | ~3×3 + 24×0.45 ≈ **~20 GB** | ✅ |
| all 12 families, 1 instance each | ~12×3 + 12×0.45 ≈ **~41 GB** | ✅ |
| matplotlib-heavy window (3.47 GB base) ×8 | ~8.7 + 8×1.3 ≈ **~19 GB** | ✅ |
| all 500 resident | ~1.5–1.9 TB | ❌ |

⇒ **the current 100 GB node disk is fine for any reasonable window**; only the
"all-resident" fantasy needs 650 GB+.

## 5. Recommendations (ties to optimizations.md)

1. **Window-following pre-pull (#9), not whole-dataset.** Pre-pull only the
   active window's families; let kubelet image GC evict the tail as the window
   slides. Node disk = working set, bounded.
2. **Pre-pull/stream the django base first** — 46% of all tasks; biggest single
   win.
3. **Isolate the heavy families** (matplotlib 3.47 GB, xarray 2.08 GB) — give
   them their own nodes/window slots so they don't blow a shared node's disk.
4. **GKE Image Streaming** for full-dataset runs — removes the resident-size
   constraint entirely (lazy layers + bounded cache).
5. **Size the window from disk** (#2/#9): `window ≲ (node_disk × 0.8 −
   bases) / diff_size`.

## 6. Methodology & caveats

- Composition: HF datasets-server, all 500 rows (5 pages × 100). Reliable.
- Sizes: Docker Hub `hub.docker.com` tag `full_size`, 300-tag sample; per-family
  medians from that sample (so small families have few samples).
- `full_size` is **compressed**; on-disk uncompressed factor (~2–2.5×) is an
  estimate, not measured here.
- Base-vs-diff split is **inferred** from pull-time ratios (81 s vs 11 s), not
  byte-measured.

## 7. To measure next

- **Exact layer bytes per family:** read the registry manifest (`/v2/.../
  manifests/<tag>` → layers[].size) when not rate-limited, or on a node via
  `crictl images` + image-store `du`, to get real base vs diff bytes and the
  uncompressed factor.
- **Cross-family layer overlap:** do families share any common base (e.g. a
  shared python/ubuntu layer)? If so, resident footprint drops further.
- Plug measured numbers back into the §4 disk model and the §2 window-sizing rule.
