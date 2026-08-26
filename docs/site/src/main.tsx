import { Asciidoc, type DocumentBlock } from '@oxide/react-asciidoc'
import {
  AsciiDocBlocks,
  attrs,
  handleDocument,
  inlineOverrides,
  loadAsciidoctor,
} from '@oxide/design-system/asciidoc'
import React, { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'

import docs from '../../oxide-karpenter.adoc?raw'
import '../styles.css'

function App() {
  const [document, setDocument] = useState<DocumentBlock | null>(null)

  useEffect(() => {
    const asciidoctor = loadAsciidoctor({})
    const loaded = asciidoctor.load(docs, { standalone: false, attributes: attrs })
    let cancelled = false

    handleDocument(loaded).then((nextDocument) => {
      if (!cancelled) setDocument(nextDocument)
    })

    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="docs-shell green-theme">
      <header className="topbar">
        <a className="wordmark" href="#top" aria-label="Karpenter Provider Oxide documentation home">
          <span className="oxide-mark">0xide</span>
          <span className="product-name">Karpenter</span>
        </a>
        <nav aria-label="Project links">
          <a href="https://github.com/oxidecomputer/karpenter-provider-oxide">GitHub ↗</a>
          <a href="https://oxide.computer">Oxide ↗</a>
        </nav>
      </header>

      <main className="content-column" id="top">
        <h1 className="page-title">Karpenter Provider Oxide</h1>
        {document ? (
          <Asciidoc
            document={document}
            options={{
              overrides: {
                admonition: AsciiDocBlocks.Admonition,
                table: AsciiDocBlocks.Table,
                section: AsciiDocBlocks.Section,
              },
              inlineOverrides,
              customDocument: AsciiDocBlocks.MinimalDocument,
            }}
          />
        ) : (
          <div className="loading">Rendering documentation…</div>
        )}
      </main>
    </div>
  )
}

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
