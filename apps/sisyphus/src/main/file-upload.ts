/** The first message carries ownership even when the file has no bytes. */
export function firstUploadMessage(file: { name: string; private: boolean; chatAttachment?: boolean }, data: Uint8Array) {
  return { name: file.name, private: file.private, chatAttachment: file.chatAttachment ?? false, data }
}
