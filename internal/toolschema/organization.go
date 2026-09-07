package toolschema

const AreaCreationDescription = "Create a Things area only when the user explicitly asks. Reuse an existing unique exact name (ignoring case) with a warning. Ambiguous names fail; do not ask a clarification question or offer choices."

const TagCreationDescription = "Create a Things tag only when the user explicitly asks, optionally under an existing exact parent tag. Reuse an existing unique exact name (ignoring case) with a warning only if its parent matches. Missing or ambiguous parents and conflicting tags fail; never reparent existing tags or offer choices."
