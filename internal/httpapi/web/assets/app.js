// Shell interactivity for the Cairn web app (ADR-0011). Written against the
// Alpine CSP build (@alpinejs/csp): directive expressions may only be bare
// property access or method calls registered here — no inline expressions — so
// the shell needs no 'unsafe-inline'/'unsafe-eval'-of-page-source in its CSP.
//
// Two view-local behaviors live here, both without a server round-trip
// (SPEC-0001 REQ "One URL Control with Copy and MCP Affordance", REQ
// "Collapsible Metadata + Comments Panel"):
//   - the URL control: toggle the shown value between the web link and the
//     mcp:// handle, and copy the active one to the clipboard; and
//   - the details panel: collapse/expand, animating grid-template-columns.
document.addEventListener('alpine:init', () => {
  window.Alpine.data('shell', () => ({
    mode: 'link',
    panelOpen: true,
    copied: false,
    link: '',
    mcp: '',

    init() {
      this.link = this.$root.dataset.link || '';
      this.mcp = this.$root.dataset.mcp || '';
    },

    isLink() { return this.mode === 'link'; },
    isMcp() { return this.mode === 'mcp'; },
    showLink() { this.mode = 'link'; },
    showMcp() { this.mode = 'mcp'; },

    notCopied() { return !this.copied; },

    copy() {
      const value = this.isLink() ? this.link : this.mcp;
      const done = () => {
        this.copied = true;
        setTimeout(() => { this.copied = false; }, 1300);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(value).then(done).catch(done);
      } else {
        done();
      }
    },

    togglePanel() { this.panelOpen = !this.panelOpen; },
    panelExpanded() { return this.panelOpen ? 'true' : 'false'; },
    panelClass() { return this.panelOpen ? 'shell-grid panel-open' : 'shell-grid panel-closed'; },
  }));
});
