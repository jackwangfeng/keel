import UIKit
import DCloudUniappRuntime

// 这个壳没有自己的原生界面：启动即进入 uni-app x（界面是原生渲染的，页面逻辑编译成
// JS 跑在系统的 JavaScriptCore 里）。生命周期照 SDK 的 UniAppXDemo 转发给 UniAppXSDK。
@main
class AppDelegate: UIResponder, UIApplicationDelegate {
    // SDK 会经 UIApplication.shared.delegate 读 window（Demo 里也是这样回填的）。
    var window: UIWindow?

    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?) -> Bool {
        UniAppXSDK.initSDK()
        UniAppXSDK.applicationDidFinishLaunchingWithOptions(application, launchOptions)
        return true
    }

    func application(_ application: UIApplication,
                     configurationForConnecting connectingSceneSession: UISceneSession,
                     options: UIScene.ConnectionOptions) -> UISceneConfiguration {
        UISceneConfiguration(name: "Default", sessionRole: connectingSceneSession.role)
    }
}
