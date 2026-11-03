// Embargoed security work (docs/security-embargo.md) lives under opaque reference names in which a
// path segment is "sec" or starts with "sec-". Magpie is public: it never lists such references and
// answers "not found" for them, so their existence is not disclosed.
export function isHiddenRef(ref: string): boolean {
  return ref
    .split("/")
    .filter((seg) => seg !== "")
    .some((seg) => seg === "sec" || seg.startsWith("sec-"));
}
