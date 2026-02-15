# Bibliography

Foundational references for the transaction model, locking strategy, and
recovery semantics used in Janus.

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
