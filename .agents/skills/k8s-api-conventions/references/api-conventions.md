> [!NOTE]
> This document is a local copy of the official Kubernetes API Conventions, fetched on 2026-05-11 from `https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md`. It is included here to ensure self-containment for tools without internet access.
> The original content is subject to the Apache License 2.0.

API Conventions
===============

*This document is oriented at users who want a deeper understanding of the
Kubernetes API structure, and developers wanting to extend the Kubernetes API.
An introduction to using resources with kubectl can be found in [the object management overview](https://kubernetes.io/docs/concepts/overview/working-with-objects/object-management/).*

**Table of Contents**

- [Types (Kinds)](#types-kinds)
  - [Resources](#resources)
  - [Objects](#objects)
    - [Metadata](#metadata)
    - [Spec and Status](#spec-and-status)
      - [Typical status properties](#typical-status-properties)
    - [References to related objects](#references-to-related-objects)
    - [Lists of named subobjects preferred over maps](#lists-of-named-subobjects-preferred-over-maps)
    - [Primitive types](#primitive-types)
    - [Constants](#constants)
    - [Unions](#unions)
  - [Lists and Simple kinds](#lists-and-simple-kinds)
- [Differing Representations](#differing-representations)
- [Verbs on Resources](#verbs-on-resources)
  - [PATCH operations](#patch-operations)
- [Short-names and Categories](#short-names-and-categories)
  - [Short-names](#short-names)
  - [Categories](#categories)
- [Idempotency](#idempotency)
- [Optional vs. Required](#optional-vs-required)
- [Nullable](#nullable)
- [Defaulting](#defaulting)
  - [Static Defaults](#static-defaults)
  - [Admission Controlled Defaults](#admission-controlled-defaults)
  - [Controller-Assigned Defaults (aka Late Initialization)](#controller-assigned-defaults-aka-late-initialization)
  - [What May Be Defaulted](#what-may-be-defaulted)
  - [Considerations For PUT Operations](#considerations-for-put-operations)
- [Concurrency Control and Consistency](#concurrency-control-and-consistency)
- [Serialization Format](#serialization-format)
- [Units](#units)
- [Selecting Fields](#selecting-fields)
- [Object references](#object-references)
  - [Naming of the reference field](#naming-of-the-reference-field)
  - [Referencing resources with multiple versions](#referencing-resources-with-multiple-versions)
  - [Handling of resources that do not exist](#handling-of-resources-that-do-not-exist)
  - [Validation of fields](#validation-of-fields)
  - [Do not modify the referred object](#do-not-modify-the-referred-object)
  - [Minimize copying or printing values to the referrer object](#minimize-copying-or-printing-values-to-the-referrer-object)
  - [Object References Examples](#object-references-examples)
    - [Single resource reference](#single-resource-reference)
      - [Controller behavior](#controller-behavior)
    - [Multiple resource reference](#multiple-resource-reference)
      - [Kind vs. Resource](#kind-vs-resource)
      - [Controller behavior](#controller-behavior-1)
    - [Generic object reference](#generic-object-reference)
      - [Controller behavior](#controller-behavior-2)
    - [Field reference](#field-reference)
      - [Controller behavior](#controller-behavior-3)
- [HTTP Status codes](#http-status-codes)
    - [Success codes](#success-codes)
    - [Error codes](#error-codes)
- [Response Status Kind](#response-status-kind)
- [Events](#events)
- [Naming conventions](#naming-conventions)
  - [Namespace Names](#namespace-names)
- [Label, selector, and annotation conventions](#label-selector-and-annotation-conventions)
- [WebSockets and SPDY](#websockets-and-spdy)
- [Validation](#validation)
- [Automatic Resource Allocation And Deallocation](#automatic-resource-allocation-and-deallocation)
- [Representing Allocated Values](#representing-allocated-values)
  - [When to use a `spec` field](#when-to-use-a-spec-field)
  - [When to use a `status` field](#when-to-use-a-status-field)
    - [Sequencing operations](#sequencing-operations)
  - [When to use a different type](#when-to-use-a-different-type)


The conventions of the [Kubernetes API](https://kubernetes.io/docs/concepts/overview/kubernetes-api/) (and related APIs in the
ecosystem) are intended to ease client development and ensure that configuration
mechanisms can be implemented that work across a diverse set of use cases
consistently.

The general style of the Kubernetes API is RESTful - clients create, update,
delete, or retrieve a description of an object via the standard HTTP verbs
(POST, PUT, DELETE, and GET) - and those APIs preferentially accept and return
JSON. Kubernetes also exposes additional endpoints for non-standard verbs and
allows alternative content types. All of the JSON accepted and returned by the
server has a schema, identified by the "kind" and "apiVersion" fields. Where
relevant HTTP header fields exist, they should mirror the content of JSON
fields, but the information should not be represented only in the HTTP header.

The following terms are defined:

* **Kind** the name of a particular object schema (e.g. the "Cat" and "Dog"
kinds would have different attributes and properties)
* **Resource** a representation of a system entity, sent or retrieved as JSON
via HTTP to the server. Resources are exposed via:
  * Collections - a list of resources of the same type, which may be queryable
  * Elements - an individual resource, addressable via a URL
* **API Group** a set of resources that are exposed together, along
with the version exposed in the "apiVersion" field as "GROUP/VERSION", e.g.
"policy.k8s.io/v1".

Each resource typically accepts and returns data of a single kind. A kind may be
accepted or returned by multiple resources that reflect specific use cases. For
instance, the kind "Pod" is exposed as a "pods" resource that allows end users
to create, update, and delete pods, while a separate "pod status" resource (that
acts on "Pod" kind) allows automated processes to update a subset of the fields
in that resource.

Resources are bound together in API groups - each group may have one or more
versions that evolve independent of other API groups, and each version within
the group has one or more resources. Group names are typically in domain name
form - the Kubernetes project reserves use of the empty group, all single
word names ("extensions", "apps"), and any group name ending in "*.k8s.io" for
its sole use. When choosing a group name, we recommend selecting a subdomain
your group or organization owns, such as "widget.mycompany.com".

Version strings should match
[DNS_LABEL](https://git.k8s.io/design-proposals-archive/architecture/identifiers.md)
format.



Resource collections should be all lowercase and plural, whereas kinds are
CamelCase and singular. Group names must be lower case and be valid DNS
subdomains.


## Types (Kinds)

Kinds are grouped into three categories:

1. **Objects** represent a persistent entity in the system.

   Creating an API object is a record of intent - once created, the system will
work to ensure that resource exists. All API objects have common metadata.

   An object may have multiple resources that clients can use to perform
specific actions that create, update, delete, or get.

   Examples: `Pod`, `ReplicationController`, `Service`, `Namespace`, `Node`.

2. **Lists** are collections of **resources** of one (usually) or more
(occasionally) kinds.

   The name of a list kind must end with "List". Lists have a limited set of
common metadata. All lists use the required "items" field to contain the array
of objects they return. Any kind that has the "items" field must be a list kind.

   Most objects defined in the system should have an endpoint that returns the
full set of resources, as well as zero or more endpoints that return subsets of
the full list. Some objects may be singletons (the current user, the system
defaults) and may not have lists.

   In addition, all lists that return objects with labels should support label
filtering (see [the labels documentation](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/)),
and most lists should support filtering by fields (see
[the fields documentation](https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/)).

   Examples: `PodList`, `ServiceList`, `NodeList`.

Note that`kubectl` and other tools sometimes output collections of resources
as `kind: List`. Keep in mind that `kind: List` is not part of the Kubernetes API; it is
exposing an implementation detail from client-side code in those tools, used to
handle groups of mixed resources.

3. **Simple** kinds are used for specific actions on objects and for
non-persistent entities.

   Given their limited scope, they have the same set of limited common metadata
as lists.

   For instance, the "Status" kind is returned when errors occur and is not
persisted in the system.

   Many simple resources are "subresources", which are rooted at API paths of
specific resources. When resources wish to expose alternative actions or views
that are closely coupled to a single resource, they should do so using new
sub-resources. Common subresources include:

   * `/binding`: Used to bind a resource representing a user request (e.g., Pod,
PersistentVolumeClaim) to a cluster infrastructure resource (e.g., Node,
PersistentVolume).
   * `/status`: Used to write just the `status` portion of a resource. For
example, the `/pods` endpoint only allows updates to `metadata` and `spec`,
since those reflect end-user intent. An automated process should be able to
modify status for users to see by sending an updated Pod kind to the server to
the "/pods/<name>/status" endpoint - the alternate endpoint allows
different rules to be applied to the update, and access to be appropriately
restricted.
   * `/scale`: Used to read and write the count of a resource in a manner that
is independent of the specific resource schema.

   Two additional subresources, `proxy` and `portforward`, provide access to
cluster resources as described in
[accessing the cluster](https://kubernetes.io/docs/tasks/access-application-cluster/access-cluster/).

The standard REST verbs (defined below) MUST return singular JSON objects. Some
API endpoints may deviate from the strict REST pattern and return resources that
are not singular JSON objects, such as streams of JSON objects or unstructured
text log data.

A common set of "meta" API objects are used across all API groups and are
thus considered part of the API group named `meta.k8s.io`. These types may
evolve independent of the API group that uses them and API servers may allow
them to be addressed in their generic form. Examples are `ListOptions`,
`DeleteOptions`, `List`, `Status`, `WatchEvent`, and `Scale`. For historical
reasons these types are part of each existing API group. Generic tools like
quota, garbage collection, autoscalers, and generic clients like kubectl
leveraging these types to define consistent behavior across different resource
types, like the interfaces in programming languages.

The term "kind" is reserved for these "top-level" API types. The term "type"
should be used for distinguishing sub-categories within objects or subobjects.

### Resources

All JSON objects returned by an API MUST have the following fields:

* kind: a string that identifies the schema this object should have
* apiVersion: a string that identifies the version of the schema the object
should have

These fields are required for proper decoding of the object. They may be
populated by the server by default from the specified URL path, but the client
likely needs to know the values in order to construct the URL path.

### Objects

#### Metadata

Every object kind MUST have the following metadata in a nested object field
called "metadata":

* namespace: a namespace is a DNS compatible label that objects are subdivided
into. The default namespace is 'default'. See
[the namespace docs](https://kubernetes.io/docs/concepts/overview/working-with-objects/namespaces/) for more.
* name: a string that uniquely identifies this object within the current
namespace (see [the identifiers docs](https://kubernetes.io/docs/concepts/overview/working-with-objects/names/)).
This value is used in the path when retrieving an individual object.
* uid: a unique in time and space value (typically an RFC 4122 generated
identifier, see [the identifiers docs](https://kubernetes.io/docs/concepts/overview/working-with-objects/names/))
used to distinguish between objects with the same name that have been deleted
and recreated

Every object SHOULD have the following metadata in a nested object field called
"metadata":

* resourceVersion: a string that identifies the internal version of this object
that can be used by clients to determine when objects have changed. This value
MUST be treated as opaque by clients and passed unmodified back to the server.
Clients should not assume that the resource version has meaning across
namespaces, different kinds of resources, or different servers. (See
[concurrency control](#concurrency-control-and-consistency), below, for more
details.)
* generation: a sequence number representing a specific generation of the
desired state. Set by the system and monotonically increasing, per-resource. May
be compared, such as for RAW and WAW consistency.
* creationTimestamp: a string representing an RFC 3339 date of the date and time
an object was created
* deletionTimestamp: a string representing an RFC 3339 date of the date and time
after which this resource will be deleted. This field is set by the server when
a graceful deletion is requested by the user, and is not directly settable by a
client. The resource will be deleted (no longer visible from resource lists, and
not reachable by name) after the time in this field except when the object has
a finalizer set. In case the finalizer is set the deletion of the object is
postponed at least until the finalizer is removed.
Once the deletionTimestamp is set, this value may not be unset or be set further
into the future, although it may be shortened or the resource may be deleted
prior to this time.
* labels: a map of string keys and values that can be used to organize and
categorize objects (see [the labels docs](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/))
* annotations: a map of string keys and values that can be used by external
tooling to store and retrieve arbitrary metadata about this object (see
[the annotations docs](https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/))

Labels are intended for organizational purposes by end users (select the pods
that match this label query). Annotations enable third-party automation and
tooling to decorate objects with additional metadata for their own use.

#### Spec and Status

By convention, the Kubernetes API makes a distinction between the specification
of the desired state of an object (a nested object field called `spec`) and the
status of the object at the current time (a nested object field called
`status`). The specification is a complete description of the desired state,
including configuration settings provided by the user,
[default values](#defaulting) expanded by the system, and properties initialized
or otherwise changed after creation by other ecosystem components (e.g.,
schedulers, auto-scalers), and is persisted in stable storage with the API
object. If the specification is deleted, the object will be purged from the
system.

The `status` summarizes the current state of the object in the system, and is
usually persisted with the object by automated processes but may be generated
on the fly.  As a general guideline, fields in `status` should be the most recent
observations of actual state, but they may contain information such as the
results of allocations or similar operations which are executed in response to
the object's `spec`.  See [below](#representing-allocated-values) for more
details.

Types with both `spec` and `status` stanzas can (and usually should) have distinct
authorization scopes for them.  This allows users to be granted full write
access to `spec` and read-only access to status, while relevant controllers are
granted read-only access to `spec` but full write access to status.

When a new version of an object is POSTed or PUT, the `spec` is updated and
available immediately. Over time the system will work to bring the `status` into
line with the `spec`. The system will drive toward the most recent `spec`
regardless of previous versions of that stanza. For example, if a value is
changed from 2 to 5 in one PUT and then back down to 3 in another PUT the system
is not required to 'touch base' at 5 before changing the `status` to 3. In other
words, the system's behavior is *level-based* rather than *edge-based*. This
enables robust behavior in the presence of missed intermediate state changes.

The Kubernetes API also serves as the foundation for the declarative
configuration schema for the system. In order to facilitate level-based
operation and expression of declarative configuration, fields in the
specification should have declarative rather than imperative names and
semantics -- they represent the desired state, not actions intended to yield the
desired state.

The PUT and POST verbs on objects MUST ignore the `status` values, to avoid
accidentally overwriting the `status` in read-modify-write scenarios. A `/status`
subresource MUST be provided to enable system components to update statuses of
resources they manage.

Otherwise, PUT expects the whole object to be specified. Therefore, if a field
is omitted it is assumed that the client wants to clear that field's value. The
PUT verb does not accept partial updates. Modification of just part of an object
may be achieved by GETting the resource, modifying part of the spec, labels, or
annotations, and then PUTting it back. See
[concurrency control](#concurrency-control-and-consistency), below, regarding
read-modify-write consistency when using this pattern. Some objects may expose
alternative resource representations that allow mutation of the status, or
performing custom actions on the object.

All objects that represent a physical resource whose state may vary from the
user's desired intent SHOULD have a `spec` and a `status`. Objects whose state
cannot vary from the user's desired intent MAY have only `spec`, and MAY rename
`spec` to a more appropriate name.

Objects that contain both `spec` and `status` should not contain additional
top-level fields other than the standard metadata fields.

Some objects which are not persisted in the system - such as `SubjectAccessReview`
and other webhook style calls - may choose to add `spec` and `status` to encapsulate
a "call and response" pattern. The `spec` is the request (often a request for
information) and the `status` is the response. For these RPC like objects the only
operation may be POST, but having a consistent schema between submission and
response reduces the complexity of these clients.

##### Typical status properties

**Conditions** provide a standard mechanism for higher-level status reporting
from a controller. They are an extension mechanism which allows tools and other
controllers to collect summary information about resources without needing to
understand resource-specific status details. Conditions should complement more
detailed information about the observed status of an object written by a
controller, rather than replace it. For example, the "Available" condition of a
Deployment can be determined by examining `readyReplicas`, `replicas`, and
other properties of the Deployment. However, the "Available" condition allows
other components to avoid duplicating the availability logic in the Deployment
controller.

Objects may report multiple conditions, and new types of conditions may be
added in the future or by 3rd party controllers. Therefore, conditions are
represented using a list/slice of objects, where each condition has a similar
structure. This collection should be treated as a map with a key of `type`.

Conditions are most useful when they follow some consistent conventions:

* Conditions should be added to explicitly convey properties that users and
  components care about rather than requiring those properties to be inferred
  from other observations.  Once defined, the meaning of a Condition can not be
  changed arbitrarily - it becomes part of the API, and has the same backwards-
  and forwards-compatibility concerns of any other part of the API.

* Controllers should apply their conditions to a resource the first time they
  visit the resource, even if the `status` is Unknown. This allows other
  components in the system to know that the condition exists and the controller
  is making progress on reconciling that resource.

   * Not all controllers will observe the previous advice about reporting
     "Unknown" or "False" values. For known conditions, the absence of a
     condition `status` should be interpreted the same as `Unknown`, and
     typically indicates that reconciliation has not yet finished (or that the
     resource state may not yet be observable).

* For some conditions, `True` represents normal operation, and for some
  conditions, `False` represents normal operation. ("Normal-true" conditions
  are sometimes said to have "positive polarity", and "normal-false" conditions
  are said to have "negative polarity".) Without further knowledge of the
  conditions, it is not possible to compute a generic summary of the conditions
  on a resource.

* Condition type names should make sense for humans; neither positive nor
  negative polarity can be recommended as a general rule. A negative condition
  like "MemoryExhausted" may be easier for humans to understand than
  "SufficientMemory". Conversely, "Ready" or "Succeeded" may be easier to
  understand than "Failed", because "Failed=Unknown" or "Failed=False" may
  cause double-negative confusion.

* Condition type names should describe the current observed state of the
  resource, rather than describing the current state transitions. This
  typically means that the name should be an adjective ("Ready", "OutOfDisk")
  or a past-tense verb ("Succeeded", "Failed") rather than a present-tense verb
  ("Deploying"). Intermediate states may be indicated by setting the `status` of
  the condition to `Unknown`.

  * For state transitions which take a long period of time (e.g. more than 1
    minute), it is reasonable to treat the transition itself as an observed
    state. In these cases, the Condition (such as "Resizing") itself should not
    be transient, and should instead be signalled using the
    `True`/`False`/`Unknown` pattern. This allows other observers to determine
    the last update from the controller, whether successful or failed. In cases
    where the state transition is unable to complete and continued
    reconciliation is not feasible, the Reason and Message should be used to
    indicate that the transition failed.

* When designing Conditions for a resource, it's helpful to have a common
  top-level condition which summarizes more detailed conditions. Simple
  consumers may simply query the top-level condition. Although they are not a
  consistent standard, the `Ready` and `Succeeded` condition types may be used
  by API designers for long-running and bounded-execution objects, respectively.

Conditions should follow the standard schema included in [k8s.io/apimachinery/pkg/apis/meta/v1/types.go](https://github.com/kubernetes/apimachinery/blob/release-1.23/pkg/apis/meta/v1/types.go#L1432-L1492).
It should be included as a top level element in status, similar to
```go
// +listType=map
// +listMapKey=type
// +patchStrategy=merge
// +patchMergeKey=type
// +optional
Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
```

The `metav1.Conditions` includes the following fields

```go
// type of condition in CamelCase or in foo.example.com/CamelCase.
// +required
Type string `json:"type" protobuf:"bytes,1,opt,name=type"`
// status of the condition, one of True, False, Unknown.
// +required
Status ConditionStatus `json:"status" protobuf:"bytes,2,opt,name=status"`
// observedGeneration represents the .metadata.generation that the condition was set based upon.
// For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
// with respect to the current state of the instance.
// +optional
ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,3,opt,name=observedGeneration"`
// lastTransitionTime is the last time the condition transitioned from one status to another.
// This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.
// +required
LastTransitionTime Time `json:"lastTransitionTime" protobuf:"bytes,4,opt,name=lastTransitionTime"`
// reason contains a programmatic identifier indicating the reason for the condition's last transition.
// Producers of specific condition types may define expected values and meanings for this field,
// and whether the values are considered a guaranteed API.
// The value should be a CamelCase string.
// This field may not be empty.
// +required
Reason string `json:"reason" protobuf:"bytes,5,opt,name=reason"`
// message is a human readable message indicating details about the transition.
// This may be an empty string.
// +required
Message string `json:"message" protobuf:"bytes,6,opt,name=message"`
```

Additional fields may be added in the future.

Use of the `Reason` field is required.

Condition types should be named in PascalCase. Short condition names are
preferred (e.g. "Ready" over "MyResourceReady").

Condition `status` values may be `True`, `False`, or `Unknown`. The absence of a
condition should be interpreted the same as `Unknown`.  How controllers handle
`Unknown` depends on the Condition in question.

The thinking around conditions has evolved over time, so there are several
non-normative examples in wide use.

In general, condition values may change back and forth, but some condition
transitions may be monotonic, depending on the resource and condition type.
However, conditions are observations and not, themselves, state machines, nor do
we define comprehensive state machines for objects, nor behaviors associated
with state transitions. The system is level-based rather than edge-triggered,
and should assume an Open World.

An example of an oscillating condition type is `Ready`, which indicates the
object was believed to be fully operational at the time it was last probed. A
possible monotonic condition could be `Succeeded`. A `True` status for
`Succeeded` would imply completion and that the resource was no longer
active. An object that was still active would generally have a `Succeeded`
condition with status `Unknown`.

Some resources in the v1 API contain fields called **`phase`**, and associated
`message`, `reason`, and other status fields. The pattern of using `phase` is
deprecated. Newer API types should use conditions instead. Phase was
essentially a state-machine enumeration field, that contradicted [system-design
principles](https://git.k8s.io/design-proposals-archive/architecture/principles.md#control-logic) and
hampered evolution, since [adding new enum values breaks backward compatibility](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api_changes.md). Rather than encouraging clients to infer
implicit properties from phases, we prefer to explicitly expose the individual
conditions that clients need to monitor. Conditions also have the benefit that
it is possible to create some conditions with uniform meaning across all
resource types, while still exposing others that are unique to specific
resource types.  See [#7856](http://issues.k8s.io/7856) for more details and
discussion.

In condition types, and everywhere else they appear in the API, **`Reason`** is
intended to be a one-word, CamelCase representation of the category of cause of
the current status, and **`Message`** is intended to be a human-readable phrase
or sentence, which may contain specific details of the individual occurrence.
`Reason` is intended to be used in concise output, such as one-line
`kubectl get` output, and in summarizing occurrences of causes, whereas
`Message` is intended to be presented to users in detailed status explanations,
such as `kubectl describe` output.

Historical information status (e.g., last transition time, failure counts) is
only provided with reasonable effort, and is not guaranteed to not be lost.

Status information that may be large (especially proportional in size to
collections of other resources, such as lists of references to other objects --
see below) and/or rapidly changing, such as
[resource usage](https://git.k8s.io/design-proposals-archive/scheduling/resources.md#usage-data), should be put into separate
objects, with possibly a reference from the original object. This helps to
ensure that GETs and watch remain reasonably efficient for the majority of
clients, which may not need that data.

Some resources report the `observedGeneration`, which is the `generation` most
recently observed by the component responsible for acting upon changes to the
desired state of the resource. This can be used, for instance, to ensure that
the reported status reflects the most recent desired status.

#### References to related objects

References to loosely coupled sets of objects, such as
[pods](https://kubernetes.io/docs/concepts/workloads/pods/) overseen by a
[replication controller](https://kubernetes.io/docs/concepts/workloads/controllers/replicationcontroller/),
are usually best referred to using a
[label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors). In order to
ensure that GETs of individual objects remain bounded in time and space, these
sets may be queried via separate API queries, but will not be expanded in the
referring object's status.

For references to specific objects, see [Object references](#object-references).

References in the `status` of the referee to the referrer may be permitted, when
the references are one-to-one and do not need to be frequently updated,
particularly in an edge-based manner.

#### Lists of named subobjects preferred over maps

Discussed in [#2004](http://issue.k8s.io/2004) and elsewhere. There are
no maps of subobjects in any API objects. Instead, the convention is to
use a list of subobjects containing name fields. These conventions, and
how one can change the semantics of lists, structs and maps are
described in more details in the Kubernetes
[documentation](https://kubernetes.io/docs/reference/using-api/server-side-apply/#merge-strategy).

For example:

```yaml
ports:
  - name: www
    containerPort: 80
```

vs.

```yaml
ports:
  www:
    containerPort: 80
```

This rule maintains the invariant that all JSON/YAML keys are fields in API
objects. The only exceptions are pure maps in the API (currently, labels,
selectors, annotations, data), as opposed to sets of subobjects.

#### Primitive types

Also read the section on validation, below.

When selecting fields, consider the following:
* Look at similar fields in the API (e.g. ports, durations) and follow the
  conventions of existing fields.
* Do not use numeric enums. Use aliases for string instead (e.g.
  `NodeConditionType`).
* All public integer fields MUST use the Go `int32` or Go `int64` types, not
  `int` (which is ambiguously sized, depending on target platform).  Internal
  types may use `int`.
* For integer fields, prefer `int32` to `int64` unless you need to represent
  values larger than `int32`.  See other guidelines about limitations of
  `int64` and language compatibility.
* Do not use unsigned integers, due to inconsistent support across languages and
  libraries. Just validate that the integer is non-negative if that's the case.
* All numbers (e.g. `int32`, `int64`) are converted to `float64` by Javascript
  and some other languages, so any field which is expected to exceed that
  either in magnitude or in precision (e.g. integer values > 53 bits)
  should be serialized and accepted as strings. `int64` fields must be
  bounds-checked to be within the range of `-(2^53) < x < (2^53)`.
* Avoid floating-point values as much as possible, and never use them in spec.
  Floating-point values cannot be reliably round-tripped (encoded and
  re-decoded) without changing, and have varying precision and representations
  across languages and architectures.
* Think twice about `bool` fields. Many ideas start as boolean but eventually
  trend towards a small set of mutually exclusive options.  Plan for future
  expansions by describing the policy options explicitly as a string type
  alias (e.g. `TerminationMessagePolicy`).

#### Constants

Some fields will have a list of allowed values (enumerations). These values will
be strings, and they will be in CamelCase, with an initial uppercase letter.
Examples: `ClusterFirst`, `Pending`, `ClientIP`. When an acronym or initialism
each letter in the acronym should be uppercase, such as with `ClientIP` or
`TCPDelay`. When a proper name or the name of a command-line executable is used
as a constant the proper name should be represented in consistent casing -
examples: `systemd`, `iptables`, `IPVS`, `cgroupfs`, `Docker` (as a generic
concept), `docker` (as the command-line executable). If a proper name is used
which has mixed capitalization like `eBPF` that should be preserved in a longer
constant such as `eBPFDelegation`.

All API within Kubernetes must leverage constants in this style, including
flags and configuration files. Where inconsistent constants were previously used,
new flags should be CamelCase only, and over time old flags should be updated to
accept a CamelCase value alongside the inconsistent constant. Example: the
Kubelet accepts a `--topology-manager-policy` flag that has values `none`,
`best-effort`, `restricted`, and `single-numa-node`. This flag should accept
`None`, `BestEffort`, `Restricted`, and `SingleNUMANode` going forward. If new
values are added to the flag, both forms should be supported.

#### Unions

Sometimes, at most one of a set of fields can be set.  For example, the
[volumes] field of a PodSpec has 17 different volume type-specific fields, such
as `nfs` and `iscsi`.  All fields in the set should be
[Optional](#optional-vs-required).

Sometimes, when a new type is created, the api designer may anticipate that a
union will be needed in the future, even if only one field is allowed initially.
In this case, be sure to make the field [Optional](#optional-vs-required)
In the validation, you may still return an error if the sole field is unset. Do
not set a default value for that field.

### Lists and Simple kinds

Every list or simple kind SHOULD have the following metadata in a nested object
field called "metadata":

* resourceVersion: a string that identifies the common version of the objects
returned by in a list. This value MUST be treated as opaque by clients and
passed unmodified back to the server. A resource version is only valid within a
single namespace on a single kind of resource.

Every simple kind returned by the server, and any simple kind sent to the server
that must support idempotency or optimistic concurrency should return this
value. Since simple resources are often used as input alternate actions that
modify objects, the resource version of the simple resource should correspond to
the resource version of the object.
