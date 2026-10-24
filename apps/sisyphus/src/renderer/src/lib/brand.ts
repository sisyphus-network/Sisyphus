// Vite rewrites this static asset URL for both HTTP development and file:// builds.
export const logoUrl = new URL('../../../../../../assets/logo.png', import.meta.url).href
export const appIconUrl = new URL('../../../../build/icons/512x512.png', import.meta.url).href
