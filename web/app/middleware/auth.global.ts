export default defineNuxtRouteMiddleware((to) => {
  const { loggedIn } = useSession()
  if (!loggedIn.value && to.path !== '/login') return navigateTo('/login')
  if (loggedIn.value && to.path === '/login') return navigateTo('/')
})
