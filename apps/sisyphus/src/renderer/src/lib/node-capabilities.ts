// Use explicit capabilities, never version-string guesses or optimistic
// protobuf compatibility: old daemons silently ignore unknown fields.
export function supportsChatFileReferences(info: { capabilities?: unknown }): boolean {
  return Array.isArray(info.capabilities)
    && info.capabilities.includes('chat-file-references-v1')
    && info.capabilities.includes('chat-attachment-retention-v1')
}
