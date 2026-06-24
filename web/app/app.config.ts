export default defineAppConfig({
  ui: {
    colors: {
      primary: 'blue',     // actions, links, primary bars
      neutral: 'slate',    // surfaces, text, chrome
      success: 'emerald',  // healthy / running / published
      warning: 'amber',    // live / transitional (cordon, drain, pending)
      error: 'red',        // lost / failed / destructive
      info: 'sky',         // info alerts — kept distinct from primary blue
    },
  },
})
