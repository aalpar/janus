# Bibliography

Foundational references for the transaction model, locking strategy,
recovery semantics, and infrastructure lineage used in Janus.

## Sagas and Compensating Transactions

Garcia-Molina, H. and Salem, K. (1987). "Sagas." *Proceedings of the
1987 ACM SIGMOD International Conference on Management of Data*,
pp. 249-259.
<https://doi.org/10.1145/38714.38742>

> Introduces the Saga pattern: a long-lived transaction broken into a
> sequence of sub-transactions, each paired with a compensating action.
> Janus's Transaction CRD is a direct implementation — `spec.changes`
> defines the forward steps, and the controller generates compensating
> actions from captured prior state.

## Transaction Processing and Atomic Commitment

Gray, J. (1978). "Notes on Data Base Operating Systems." In: Bayer, R.,
Graham, R.M., Seegmuller, G. (eds) *Operating Systems: An Advanced
Course*. Lecture Notes in Computer Science, vol 60. Springer, Berlin,
Heidelberg. pp. 393-481.

> Formalizes the transaction concept (ACID properties) and the two-phase
> commit protocol. Janus's Prepare/Commit phases mirror 2PC structure
> but use Saga-style compensation instead of blocking participants.

Gray, J. and Reuter, A. (1993). *Transaction Processing: Concepts and
Techniques*. Morgan Kaufmann Publishers. ISBN 1-55860-190-2.

> Comprehensive treatment of transaction processing — commit protocols,
> recovery, logging, and locking. The "one item per reconcile cycle"
> strategy in Janus draws from the write-ahead logging discipline: persist
> progress before advancing.

Bernstein, P.A., Hadzilacos, V., and Goodman, N. (1987). *Concurrency
Control and Recovery in Database Systems*. Addison-Wesley. Chapter 7:
Distributed Commit.

> Foundational theory of distributed commit protocols and their failure
> modes. Janus's lock-then-capture-then-mutate sequencing follows
> the strict two-phase locking discipline described here.

## Crash Recovery in Distributed Systems

Lampson, B. and Sturgis, H. (1979). "Crash Recovery in a Distributed
Data Storage System." Xerox Palo Alto Research Center, Technical
Report.

> Establishes principles for making distributed systems crash-recoverable
> through persistent state. Janus applies this by persisting per-item
> status to the Kubernetes API server after each reconcile cycle,
> enabling resume-from-last-checkpoint after controller restarts.

## Non-Blocking Commitment

Skeen, D. (1981). "Nonblocking Commit Protocols." *Proceedings of the
1981 ACM SIGMOD International Conference on Management of Data*,
pp. 133-142.

> Defines the blocking problem in 2PC and conditions for non-blocking
> protocols. Janus sidesteps blocking by using advisory Lease locks
> with expiration — a crashed coordinator's locks eventually expire,
> allowing other transactions to proceed.

Guerraoui, R. and Schiper, A. (1995). "The Decentralized Non-Blocking
Atomic Commitment Protocol." *Proceedings of the 7th IEEE Symposium
on Parallel and Distributed Processing*, pp. 2-9.

> Explores decentralizing the coordinator role in atomic commitment.
> Relevant to Janus's design because the Kubernetes controller-runtime
> reconciliation loop acts as a single-writer coordinator — decentralized
> variants could inform future multi-controller architectures.

Babaoglu, O. and Toueg, S. (1993). "Understanding Non-Blocking Atomic
Commitment." In: Mullender, S. (ed) *Distributed Systems*, 2nd ed.
Addison-Wesley. pp. 147-168.

> Survey of non-blocking atomic commitment, relating it to consensus.
> Contextualizes why Janus chooses advisory locking over consensus-based
> commitment — the single reconciler loop avoids the need for distributed
> agreement.

## Consensus and Transaction Commit

Gray, J. and Lamport, L. (2006). "Consensus on Transaction Commit."
*ACM Transactions on Database Systems*, vol 31, no 1, pp. 133-160.
<https://doi.org/10.1145/1132863.1132867>

> Proves that transaction commit is equivalent to consensus and proposes
> Paxos Commit. Janus's single-controller design avoids requiring
> consensus by relying on the Kubernetes API server (backed by etcd/Raft)
> as the sole source of truth for transaction state.

## Lease-Based Fault Tolerance

Gray, C.G. and Cheriton, D.R. (1989). "Leases: An Efficient
Fault-Tolerant Mechanism for Distributed File Cache Consistency."
*Proceedings of the 12th ACM Symposium on Operating Systems Principles
(SOSP)*, pp. 202-210.
<https://doi.org/10.1145/74850.74870>

> Introduces time-bounded distributed locks: a lease grants a holder
> exclusive access for a bounded duration, and if the holder fails to
> renew before expiry, other participants can acquire it. Kubernetes
> `Lease` objects (`coordination.k8s.io/v1`) are a direct implementation
> of this concept. Janus acquires a Lease per resource before mutation,
> renews before each commit step, and relies on expiration to unblock
> other transactions if the controller crashes. The paper's core insight
> — that non-Byzantine failures affect performance, not correctness —
> is exactly the trade-off Janus makes with advisory locking.

## Optimistic Concurrency Control

Kung, H.T. and Robinson, J.T. (1981). "On Optimistic Methods for
Concurrency Control." *ACM Transactions on Database Systems*, vol 6,
no 2, pp. 213-226.
<https://doi.org/10.1145/319566.319567>

> Introduces the read-validate-write pattern as an alternative to
> locking. Kubernetes adopted this as `resourceVersion` semantics:
> clients read state, compute changes, and submit updates that include
> the observed version; the API server rejects stale writes with a 409
> Conflict. Every status update in Janus's reconciliation loop is an
> optimistic write — if a concurrent reconciliation has advanced the
> Transaction's state, the controller re-reads and retries. This paper
> also informs a current gap in Janus: detecting resource modifications
> between prepare and commit would apply the same read-validate-write
> discipline to the target resources themselves.

## Distributed Coordination Services

Burrows, M. (2006). "The Chubby Lock Service for Loosely-Coupled
Distributed Systems." *Proceedings of the 7th USENIX Symposium on
Operating Systems Design and Implementation (OSDI)*, pp. 335-350.
<https://www.usenix.org/conference/osdi-06/chubby-lock-service-loosely-coupled-distributed-systems>

> Demonstrates that coarse-grained advisory locks plus a reliable
> small-file store are sufficient for coordinating distributed systems
> at Google scale. Chubby influenced ZooKeeper, which influenced etcd,
> which became the Kubernetes state store. Janus's Lease-based locking
> is a lighter version of Chubby's advisory locks — Kubernetes Leases
> explicitly omit Chubby's fencing guarantees, making the operator
> responsible for handling the brief window where stale lock holders
> might still act. Janus addresses this through `resourceVersion`-based
> optimistic concurrency on each state transition.

## Consensus Protocols

Ongaro, D. and Ousterhout, J. (2014). "In Search of an Understandable
Consensus Algorithm." *Proceedings of the 2014 USENIX Annual Technical
Conference (ATC)*, pp. 305-319.
<https://raft.github.io/raft.pdf>

> Introduces the Raft consensus protocol, designed for understandability
> over Paxos. etcd implements Raft for replicated consensus, and etcd
> is the sole persistent store behind the Kubernetes API server. Every
> `resourceVersion` that Janus reads or writes is a Raft-committed
> revision in etcd. Raft's linearizable writes are what make Janus's
> one-item-per-reconcile strategy safe: once a status update returns
> success, the progress is durable across controller restarts.

## Cluster Management Lineage

The infrastructure Janus runs on has a direct intellectual lineage from
Google's internal cluster management systems. These papers document the
design decisions that became Kubernetes conventions — and explain why
the patterns Janus uses (reconciliation loops, advisory locking,
optimistic concurrency) are the idiomatic choices.

Verma, A., Pedrosa, L., Korupolu, M., Oppenheimer, D., Tune, E., and
Wilkes, J. (2015). "Large-Scale Cluster Management at Google with
Borg." *Proceedings of the 10th European Conference on Computer Systems
(EuroSys)*, Article 18.
<https://doi.org/10.1145/2741948.2741964>

> Documents Borg's architecture: declarative job specifications and
> a control loop (the Borgmaster) that continuously reconciles declared
> intent against observed cluster state. This reconciliation model
> became the Kubernetes controller's `Reconcile()` function. Janus
> inherits the pattern directly — each reconcile invocation advances
> the saga by one step, and progress state lives in the Transaction CR,
> not in controller memory.

Schwarzkopf, M., Konwinski, A., Abd-El-Malek, M., and Wilkes, J.
(2013). "Omega: Flexible, Scalable Schedulers for Large Compute
Clusters." *Proceedings of the 8th ACM European Conference on Computer
Systems (EuroSys)*, pp. 351-364.
<https://doi.org/10.1145/2465351.2465386>

> Omega replaced Borg's monolithic scheduler with a shared-state
> architecture: all logic lives in clients, and the central store
> enforces only optimistic concurrency control. Kubernetes adopted this
> wholesale — etcd is the passive shared store, the API server mediates
> access, and controllers are independent clients that read, compute,
> and write back with conflict detection. Janus operates as one such
> client: each saga step is a compare-and-swap against the Transaction
> CR, and conflicts trigger re-reconciliation rather than blocking.

Burns, B., Grant, B., Oppenheimer, D., Brewer, E., and Wilkes, J.
(2016). "Borg, Omega, and Kubernetes: Lessons Learned from Three
Container-Management Systems over a Decade." *ACM Queue*, vol 14, no 1,
pp. 70-93.
<https://doi.org/10.1145/2898442.2898444>

> The synthesis paper tracing the design lineage across all three
> systems. Three concepts it articulates are directly relevant to
> Janus's design:
>
> *Level-triggered over edge-triggered reconciliation.* Controllers
> respond to current observed state, not to a sequence of events. After
> a crash, Janus re-reads the Transaction CR and resumes from wherever
> the saga actually is rather than replaying missed events.
>
> *Shared persistent store with optimistic concurrency.* Inherited from
> Omega — the API server is a thin policy layer over etcd, and all
> coordination happens through versioned reads and conditional writes.
>
> *Decentralized control via independent controllers.* Each controller
> owns a specific concern. Janus is one such controller, reconciling
> Transaction CRs independently without requiring coordination with
> other operators.
