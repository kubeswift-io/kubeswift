## Context

SwiftImage status today has Phase, Conditions, and PreparedArtifact. The format pipeline (source → import → prepare) is implicit: spec.format exists but status does not record what was actually imported or what format the prepared artifact uses. Cloud Hypervisor requires raw; qcow2 sources are converted during import.

## Goals / Non-Goals

**Goals:**
- Add sourceFormat and preparedFormat to SwiftImageStatus for observability
- Use existing DiskFormat type (raw, qcow2)
- Regenerate CRD and apply

**Non-Goals:**
- Controller logic to populate these fields (follow-up)
- Changing import or preparation behavior

## Decisions

**Decision:** Add two optional status fields (omitempty) rather than extending PreparedArtifactRef.

**Rationale:** PreparedArtifactRef already has Format; sourceFormat is distinct (input format). Keeping them at top-level status is clearer for observability.

## Risks / Trade-offs

- **Risk:** Fields may be empty until controller populates them. **Mitigation:** omitempty; no breaking change for existing clusters.
