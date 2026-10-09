import { memo, type ComponentProps, type HTMLAttributes } from 'react'
import { Streamdown } from 'streamdown'
import { cjk } from '@streamdown/cjk'
import { code } from '@streamdown/code'
import { math } from '@streamdown/math'
import { mermaid } from '@streamdown/mermaid'
import { cn } from '@/lib/utils'

export function Message({ from, className, ...props }: HTMLAttributes<HTMLDivElement> & { from: 'user' | 'assistant' }) {
  return <div className={cn('group flex w-full max-w-[95%] flex-col gap-2', from === 'user' ? 'is-user me-auto justify-end' : 'is-assistant', className)} {...props} />
}

export function MessageContent({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('flex w-fit min-w-0 max-w-full flex-col gap-2 overflow-visible text-sm group-[.is-user]:me-auto group-[.is-assistant]:text-foreground', className)} {...props} />
}

const plugins = { cjk, code, math, mermaid }

// Resolve each prose block independently in mixed-language replies. Code keeps
// its explicit LTR styling, while paragraphs and list markers follow their text.
const components: ComponentProps<typeof Streamdown>['components'] = {
  p: ({ children, ...props }) => <p {...props} dir="auto">{children}</p>,
  h1: ({ children, ...props }) => <h1 {...props} dir="auto">{children}</h1>,
  h2: ({ children, ...props }) => <h2 {...props} dir="auto">{children}</h2>,
  h3: ({ children, ...props }) => <h3 {...props} dir="auto">{children}</h3>,
  h4: ({ children, ...props }) => <h4 {...props} dir="auto">{children}</h4>,
  h5: ({ children, ...props }) => <h5 {...props} dir="auto">{children}</h5>,
  h6: ({ children, ...props }) => <h6 {...props} dir="auto">{children}</h6>,
  ul: ({ children, ...props }) => <ul {...props} dir="auto">{children}</ul>,
  ol: ({ children, ...props }) => <ol {...props} dir="auto">{children}</ol>,
  li: ({ children, ...props }) => <li {...props} dir="auto">{children}</li>,
  blockquote: ({ children, ...props }) => <blockquote {...props} dir="auto">{children}</blockquote>,
}

export const MessageResponse = memo(({ className, ...props }: ComponentProps<typeof Streamdown>) => (
  <Streamdown className={cn('size-full [&>*:first-child]:mt-0 [&>*:last-child]:mb-0', className)} plugins={plugins} components={components} {...props} />
))

MessageResponse.displayName = 'MessageResponse'
